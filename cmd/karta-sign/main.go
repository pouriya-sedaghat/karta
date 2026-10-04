// Command karta-sign is the producer's tool for Karta's online snapshot
// source: it creates Ed25519 signing keys and signs snapshot manifests.
// It runs where snapshots are produced (an extraction host), never in the
// Karta deployment, which holds only public keys.
//
//	karta-sign keygen --out key.pem
//	karta-sign pubkey --key key.pem
//	karta-sign sign --signer ID=key.pem [--signer ID2=key2.pem] --region region.json \
//	    --snapshot FILE [--provenance FILE] --snapshot-url URL [--provenance-url URL] \
//	    --serial N --valid-for 168h [--issued-at RFC3339] [--out manifest.dsse.json]
//	karta-sign verify --source source.json --region region.json manifest.dsse.json
//
// sign verifies the snapshot exactly as Karta's importer does (complete PBF
// scan, region box, provenance sidecar, trusted data timestamp) before it
// signs, so a manifest never vouches for a file Karta would refuse; the
// signed data timestamp is the one the importer derives.
//
// Keys are PKCS#8 PEM files, interchangeable with OpenSSL
// (`openssl genpkey -algorithm ed25519 -out key.pem`). The public key for
// the source file is the raw 32 bytes in base64:
// `openssl pkey -in key.pem -pubout -outform DER | tail -c 32 | base64`.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pouriya-sedaghat/karta/internal/importer"
	"github.com/pouriya-sedaghat/karta/internal/online"
	"github.com/pouriya-sedaghat/karta/internal/region"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "pubkey":
		err = pubkey(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "karta-sign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: karta-sign keygen --out FILE | pubkey --key FILE | sign --signer ID=FILE ... | verify --source FILE --region FILE ENVELOPE")
	os.Exit(2)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "private key file to create (PKCS#8 PEM, mode 0600)")
	_ = fs.Parse(args)
	if *out == "" {
		return errors.New("--out is required")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- operator-named output
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return printPublic(pub)
}

func printPublic(pub ed25519.PublicKey) error {
	b, _ := json.MarshalIndent(map[string]string{
		"ed25519_public_key": base64.StdEncoding.EncodeToString(pub),
		"fingerprint":        online.Fingerprint(pub),
	}, "", "  ")
	fmt.Println(string(b))
	return nil
}

func loadKey(path string) (ed25519.PrivateKey, error) { return online.LoadSigningKey(path) }

func pubkey(args []string) error {
	fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
	key := fs.String("key", "", "private key file")
	_ = fs.Parse(args)
	k, err := loadKey(*key)
	if err != nil {
		return err
	}
	return printPublic(k.Public().(ed25519.PublicKey))
}

type signers []string

func (s *signers) String() string     { return strings.Join(*s, ",") }
func (s *signers) Set(v string) error { *s = append(*s, v); return nil }

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	var sg signers
	fs.Var(&sg, "signer", "ID=KEYFILE; repeat to sign with several keys during a rotation")
	regionPath := fs.String("region", "", "region file the snapshot is for")
	snapshot := fs.String("snapshot", "", "snapshot (.osm.pbf)")
	prov := fs.String("provenance", "", "provenance sidecar (default: <snapshot>.provenance.json if present)")
	snapURL := fs.String("snapshot-url", "", "URL of the snapshot, absolute or relative to the manifest URL")
	provURL := fs.String("provenance-url", "", "URL of the sidecar (default: snapshot URL + .provenance.json)")
	serial := fs.Int64("serial", 0, "manifest serial, higher than every serial published before")
	validFor := fs.Duration("valid-for", 7*24*time.Hour, "validity of the manifest from issued-at")
	issued := fs.String("issued-at", "", "issue time (RFC 3339, default now)")
	out := fs.String("out", "", "output envelope (default stdout)")
	_ = fs.Parse(args)
	if len(sg) == 0 || *regionPath == "" || *snapshot == "" || *snapURL == "" || *serial < 1 {
		return errors.New("--signer, --region, --snapshot, --snapshot-url and --serial >= 1 are required")
	}
	reg, err := region.Load(*regionPath)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if *issued != "" {
		if now, err = time.Parse(time.RFC3339, *issued); err != nil {
			return fmt.Errorf("--issued-at: %w", err)
		}
	}
	if *prov == "" {
		if _, err := os.Lstat(*snapshot + ".provenance.json"); err == nil {
			*prov = *snapshot + ".provenance.json"
		}
	}
	v, err := importer.Verify(context.Background(), importer.VerifyOptions{SnapshotPath: *snapshot, ProvenancePath: *prov, Region: reg,
		MaxInputBytes: 1 << 42, MaxFutureSkew: 10 * time.Minute, Now: func() time.Time { return now },
		Authorize: func(context.Context, string, string, int64) (string, error) { return "the producer's signature", nil }})
	if err != nil {
		return fmt.Errorf("the snapshot would not pass Karta's input verification: %w", err)
	}
	m := online.Manifest{Format: online.ManifestFormat, RegionID: reg.ID, BBox: online.BBox(reg.BBox), Serial: *serial,
		IssuedAt: now.UTC().Truncate(time.Second), ExpiresAt: now.Add(*validFor).UTC().Truncate(time.Second),
		Snapshot: online.Snapshot{URL: *snapURL, SHA256: v.Info.SHA256, SizeBytes: v.Info.Size, DataTimestamp: v.Source.DataTimestamp.UTC()}}
	if *prov != "" {
		b, err := os.ReadFile(*prov) // #nosec G304 -- operator-named sidecar
		if err != nil {
			return err
		}
		u := *provURL
		if u == "" {
			u = *snapURL + ".provenance.json"
		}
		m.Provenance = &online.File{URL: u, SHA256: v.ProvenanceSHA256, SizeBytes: int64(len(b))}
	}
	var ss []online.Signer
	for _, s := range sg {
		id, file, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("--signer %q: want ID=KEYFILE", s)
		}
		k, err := loadKey(file)
		if err != nil {
			return err
		}
		ss = append(ss, online.Signer{KeyID: id, Key: k})
	}
	env, err := online.Sign(m, ss...)
	if err != nil {
		return err
	}
	env = append(env, '\n')
	if *out == "" {
		_, err = os.Stdout.Write(env)
		return err
	}
	return os.WriteFile(*out, env, 0o644) // #nosec G306 -- a signed manifest is public
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	srcPath := fs.String("source", "", "source file with the trusted keys")
	regionPath := fs.String("region", "", "region file")
	_ = fs.Parse(args)
	if *srcPath == "" || *regionPath == "" || fs.NArg() != 1 {
		return errors.New("--source, --region and one envelope file are required")
	}
	src, err := online.LoadSource(*srcPath)
	if err != nil {
		return err
	}
	reg, err := region.Load(*regionPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(fs.Arg(0)) // #nosec G304 -- operator-named file
	if err != nil {
		return err
	}
	v, err := online.Verify(raw, online.VerifyOptions{Source: src, Region: reg, Now: time.Now(), Skew: 10 * time.Minute})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{"key_id": v.KeyID, "key_fingerprint": v.KeyFingerprint,
		"envelope_sha256": v.EnvelopeSHA256, "manifest": v.Manifest}, "", "  ")
	fmt.Println(string(b))
	return nil
}
