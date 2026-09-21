package store

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

func TestMigrateFromV1AndConcurrentOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	// Simulate a database created by an older binary (schema v1 only).
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0] + `PRAGMA user_version = 1;`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			st.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Tx(func(tx *Tx) error { return tx.Put(Session{ID: "x", Name: "n", Status: Idle}) }); err != nil {
		t.Fatal(err)
	}
	if s, err := st.Find("x"); err != nil || s.Name != "n" {
		t.Fatalf("got %+v, %v", s, err)
	}
}
