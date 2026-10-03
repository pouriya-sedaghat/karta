package intake

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Preflight problem codes.
const (
	ProblemMissing        = "landing_missing"
	ProblemSymlink        = "symlink"
	ProblemNotDirectory   = "not_a_directory"
	ProblemOwner          = "wrong_owner"
	ProblemWorldWritable  = "writable_by_others"
	ProblemGroupWritable  = "writable_by_group"
	ProblemFilesystem     = "unsupported_filesystem"
	ProblemParentOwner    = "parent_wrong_owner"
	ProblemParentWritable = "parent_writable"
	ProblemParentSymlink  = "parent_symlink"
)

// localFilesystems are the local POSIX filesystems a landing area may be
// on: their owner, mode, inode and rename semantics are the kernel's own.
var localFilesystems = map[int64]string{
	0xEF53:     "ext2/ext3/ext4",
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0x2FC12FC1: "zfs",
	0xF2F52010: "f2fs",
	0x01021994: "tmpfs",
}

// knownRefused names filesystems refused because another system decides who
// may write them, or their Unix owner and mode bits are synthesized, or
// their inode and mtime semantics are weak: hypervisor and host shares,
// network and FUSE filesystems, Windows filesystems.
var knownRefused = map[int64]string{
	0x786f4256: "vboxsf (VirtualBox shared folder)",
	0xFF534D42: "cifs (SMB share)",
	0xFE534D42: "smb2 (SMB share)",
	0x517B:     "smb",
	0x6969:     "nfs",
	0x65735546: "fuse (also virtiofs, vmhgfs-fuse, sshfs)",
	0x01021997: "9p (also WSL drvfs)",
	0x5346544e: "ntfs",
	0x7366746e: "ntfs3",
	0x4d44:     "vfat/msdos",
	0x2011BAB0: "exfat",
	0x794c7630: "overlay (a container's own filesystem, not a mounted landing area)",
	0x00C36400: "ceph",
	0x5346414F: "afs",
}

// FilesystemName names a statfs type.
func FilesystemName(t int64) string {
	if n, ok := localFilesystems[t]; ok {
		return n
	}
	if n, ok := knownRefused[t]; ok {
		return n
	}
	return fmt.Sprintf("unknown (0x%x)", t)
}

// PreflightOptions configure the landing checks.
type PreflightOptions struct {
	// Dir is the landing directory, as the checking process sees it.
	Dir string
	// OwnerUID must own the landing directory and every delivery.
	OwnerUID uint32
	// WriterGID, when set, is the one group that may have write permission
	// on the landing directory (the landing writers); otherwise group write
	// is refused.
	WriterGID *uint32
	// Root, when set, is where the host's root is mounted (karta intake
	// check --host-root): Dir is then a host path, and its parents are
	// checked too. Inside the watcher's container only the bind-mounted
	// directory itself is the host's; its parents are the container's.
	Root string
	// Statfs returns the filesystem type of a path (a test seam).
	Statfs func(path string) (int64, error)
	Now    func() time.Time
}

func statfsType(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil // int32 on some platforms
}

// RunPreflight checks the landing area and reports every problem found; it
// passes only with none. It never opens a delivery.
func RunPreflight(o PreflightOptions) Preflight {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	statfs := statfsType
	if o.Statfs != nil {
		statfs = o.Statfs
	}
	p := Preflight{CheckedAt: now().UTC(), Problems: []Problem{}}
	add := func(code, path, format string, args ...any) {
		p.Problems = append(p.Problems, Problem{Code: code, Path: path, Detail: fmt.Sprintf(format, args...)})
	}
	real := filepath.Join(o.Root, o.Dir)
	fi, err := os.Lstat(real)
	switch {
	case err != nil:
		add(ProblemMissing, o.Dir, "%v", err)
		return p
	case fi.Mode()&fs.ModeSymlink != 0:
		add(ProblemSymlink, o.Dir, "the landing directory is a symbolic link")
		return p
	case !fi.IsDir():
		add(ProblemNotDirectory, o.Dir, "the landing area is not a directory (%s)", fi.Mode().Type())
		return p
	}
	checkDir := func(path string, info fs.FileInfo, parent bool) {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			add(ProblemOwner, path, "owner unknown")
			return
		}
		mode := info.Mode().Perm()
		ownerCode, writeCode := ProblemOwner, ProblemWorldWritable
		if parent {
			ownerCode, writeCode = ProblemParentOwner, ProblemParentWritable
		}
		switch {
		case !parent && st.Uid != o.OwnerUID:
			add(ownerCode, path, "owned by UID %d, not the landing owner %d (KARTA_INTAKE_LANDING_UID)", st.Uid, o.OwnerUID)
		case parent && st.Uid != 0 && st.Uid != o.OwnerUID:
			add(ownerCode, path, "owned by UID %d: a parent must belong to root or the landing owner, or its owner can replace the landing area", st.Uid)
		}
		if mode&0o002 != 0 {
			add(writeCode, path, "writable by every user (mode %04o)", mode)
		}
		if mode&0o020 != 0 && (o.WriterGID == nil || st.Gid != *o.WriterGID) {
			code := ProblemGroupWritable
			if parent {
				code = ProblemParentWritable
			}
			add(code, path, "writable by group %d (mode %04o), which is not the configured landing writer group (KARTA_INTAKE_WRITER_GID)", st.Gid, mode)
		}
	}
	checkDir(o.Dir, fi, false)
	t, err := statfs(real)
	if err != nil {
		add(ProblemFilesystem, o.Dir, "cannot determine the filesystem: %v", err)
	} else {
		p.FSType = FilesystemName(t)
		if _, ok := localFilesystems[t]; !ok {
			add(ProblemFilesystem, o.Dir, "the landing area is on %s, not a local POSIX filesystem; a shared, network or FUSE folder is only "+
				"untrusted transfer space: use the authenticated command (karta intake submit) from there", p.FSType)
		}
	}
	// Parents: none may be a symlink, owned by another user, or writable by
	// others (who could then rename the landing area away and replace it).
	for dir := filepath.Dir(filepath.Clean(o.Dir)); ; dir = filepath.Dir(dir) {
		pi, err := os.Lstat(filepath.Join(o.Root, dir))
		switch {
		case err != nil:
			add(ProblemMissing, dir, "%v", err)
		case pi.Mode()&fs.ModeSymlink != 0:
			add(ProblemParentSymlink, dir, "a parent of the landing area is a symbolic link")
		default:
			checkDir(dir, pi, true)
		}
		if dir == "/" || dir == "." || !strings.Contains(dir, "/") {
			break
		}
	}
	p.OK = len(p.Problems) == 0
	return p
}
