package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotationCheckerConfigAuthorityAndAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	original := &Config{Name: "original", RegistrationToken: "checker-old", Listen: "127.0.0.1:1", Tmux: "off", Agents: map[string]Agent{"custom": {}}}
	if err := Save("relay.toml", original); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load("relay.toml")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	changed := &Config{Name: "changed", RegistrationToken: "checker-new", Listen: "0.0.0.0:9", Tmux: "on"}
	if err := Save(filepath.Join(dir, "relay.toml"), changed); err != nil {
		t.Fatal(err)
	}
	token, err := loaded.CurrentRegistrationToken()
	if err != nil || token != changed.RegistrationToken {
		t.Fatalf("did not follow original absolute path: %v", err)
	}
	if loaded.Name != original.Name || loaded.Listen != original.Listen || loaded.Tmux != original.Tmux || len(loaded.Agents) != 1 || loaded.RegistrationToken != original.RegistrationToken {
		t.Fatal("reloaded non-authority configuration")
	}
	if token, err := original.CurrentRegistrationToken(); err != nil || token != original.RegistrationToken {
		t.Fatal("programmatic configuration changed")
	}
}

func TestRotationCheckerConfigFailsClosed(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "wrong-type", "empty", "absent"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "relay.toml")
			if err := Save(path, &Config{RegistrationToken: "checker-old"}); err != nil {
				t.Fatal(err)
			}
			loaded, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				err = os.Remove(path)
			} else {
				body := map[string]string{"malformed": "registration_token = [", "wrong-type": "registration_token = 42", "empty": "registration_token = \"\"", "absent": "name = \"without-token\""}[kind]
				err = os.WriteFile(path, []byte(body), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			token, err := loaded.CurrentRegistrationToken()
			if token != "" {
				t.Fatal("failed configuration returned authority")
			}
			if (kind == "missing" || kind == "malformed" || kind == "wrong-type") && err == nil {
				t.Fatal("invalid source did not report error")
			}
		})
	}
}

func TestRotationCheckerAtomicSaveReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "relay.toml")
	a := &Config{Name: "a", RegistrationToken: "checker-a", AllowedOrigins: []string{strings.Repeat("a", 32768)}}
	b := &Config{Name: "b", RegistrationToken: "checker-b", AllowedOrigins: []string{strings.Repeat("b", 32768)}}
	if err := Save(path, a); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				current, err := Load(path)
				if err != nil {
					errs <- err
					return
				}
				if current.Name != "a" && current.Name != "b" {
					errs <- fmt.Errorf("invalid name")
					return
				}
				if current.RegistrationToken != "checker-"+current.Name || len(current.AllowedOrigins) != 1 || current.AllowedOrigins[0] != strings.Repeat(current.Name, 32768) {
					errs <- fmt.Errorf("partial or mixed config")
					return
				}
				token, err := loaded.CurrentRegistrationToken()
				if err != nil || (token != "checker-a" && token != "checker-b") {
					errs <- fmt.Errorf("authority interrupted: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 120; i++ {
		next := a
		if i%2 == 0 {
			next = b
		}
		if err := Save(path, next); err != nil {
			t.Error(err)
			break
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("config mode %o", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0700 {
		t.Fatalf("new directory mode %o", di.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "relay.toml" {
		t.Fatal("save leaked temporary files")
	}
}

func TestRotationCheckerSaveFailureAndReplacementMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.toml")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, &Config{RegistrationToken: "checker"}); err == nil {
		t.Fatal("rename over directory unexpectedly succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatal("failed save damaged destination or leaked temp file")
	}
	file := filepath.Join(dir, "wide.toml")
	if err := os.WriteFile(file, []byte("name=\"old\""), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Save(file, &Config{RegistrationToken: "checker"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("replacement mode %o", info.Mode().Perm())
	}
}
