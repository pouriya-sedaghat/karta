package dbconn

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A pool reads the password file for every new connection: a rotated
// password applies without a restart.
func TestPoolReadsThePasswordPerConnection(t *testing.T) {
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Params{Host: "127.0.0.1", Port: 1, User: "karta_api", PasswordFile: pwFile, SSLMode: "disable"}
	pool, err := p.Pool(context.Background(), "karta_registry") // connects lazily
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	before := pool.Config().BeforeConnect
	cc := &pgx.ConnConfig{}
	if err := before(context.Background(), cc); err != nil || cc.Password != "old" {
		t.Fatalf("%q %v", cc.Password, err)
	}
	if err := os.WriteFile(pwFile, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := before(context.Background(), cc); err != nil || cc.Password != "new" {
		t.Fatalf("after rotation: %q %v", cc.Password, err)
	}
	if err := os.WriteFile(pwFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := before(context.Background(), cc); err == nil {
		t.Fatal("an empty password file was accepted")
	}
}
