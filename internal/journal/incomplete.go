package journal

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// MarkIncomplete persists a known per-block recording loss without marking
// unrelated streams or the storage writer itself as failed.
func (s *Store) MarkIncomplete(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("blocks"))
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("unknown block: %s", id)
		}
		var block Block
		if err := json.Unmarshal(v, &block); err != nil {
			return err
		}
		block.HistoryIncomplete = true
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), raw)
	})
}

// Reactivate marks a block whose process a holder kept alive across a daemon
// restart as active again. HistoryIncomplete is left as recorded: the epoch
// was unclean, and the adopted event carries what the holder could keep.
func (s *Store) Reactivate(id string, pid int) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("blocks"))
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("unknown block: %s", id)
		}
		var block Block
		if err := json.Unmarshal(v, &block); err != nil {
			return err
		}
		block.State = "active"
		block.PID = pid
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		return b.Put([]byte(id), raw)
	})
}
