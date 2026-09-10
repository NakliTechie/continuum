package legacy

import (
	"github.com/NakliTechie/continuum/internal/config"
	"path/filepath"
	"testing"
)

func TestO5FirstRunTokenRotation(t *testing.T) {
	for _, firstRun := range []bool{false, true} {
		label := "existing_config_positive"
		if firstRun {
			label = "first_run_config"
		}
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "relay.toml")
			if !firstRun {
				initial, err := config.Default()
				if err != nil {
					t.Fatal(err)
				}
				if err = config.Save(path, initial); err != nil {
					t.Fatal(err)
				}
			}
			running := ensureConfig(path)
			updated, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			updated.RegistrationToken = "o5-local-only-rotated-token"
			if err = config.Save(path, updated); err != nil {
				t.Fatal(err)
			}
			actual, err := running.CurrentRegistrationToken()
			if err != nil {
				t.Fatal(err)
			}
			if actual != updated.RegistrationToken {
				t.Fatal("running first-use config retains the old registration authority after persisted rotation")
			}
			t.Log("running config observed persisted token rotation")
		})
	}
}
