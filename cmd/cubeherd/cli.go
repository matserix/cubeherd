package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"cubeherd/internal/access"
	"cubeherd/internal/auth"
)

// runCLI handles the maintenance commands that work on the accounts database directly:
//
//	cubeherd user reset-owner [name]   create or repair an owner and set a new password
//	cubeherd db backup <file>          write a consistent copy of cubeherd.db
func runCLI(dataDir string, args []string) error {
	dbPath := filepath.Join(dataDir, "cubeherd.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no accounts database at %s (accounts mode was never switched on)", dbPath)
	}
	st, err := access.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	switch {
	case len(args) >= 2 && args[0] == "user" && args[1] == "reset-owner":
		name := "admin"
		if len(args) > 2 {
			name = args[2]
		}
		return resetOwner(st, name)
	case len(args) == 3 && args[0] == "db" && args[1] == "backup":
		if err := st.Snapshot(args[2]); err != nil {
			return err
		}
		fmt.Println("written", args[2])
		return nil
	}
	return errors.New("usage: cubeherd user reset-owner [name] | cubeherd db backup <file>")
}

func resetOwner(st *access.Store, name string) error {
	pw := os.Getenv("CUBEHERD_NEW_PASSWORD")
	generated := pw == ""
	if generated {
		b := make([]byte, 15)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		pw = base64.RawURLEncoding.EncodeToString(b)
	}
	if len(pw) < 10 {
		return errors.New("the password needs at least 10 characters")
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	u, _, err := st.UserByName(name)
	switch {
	case errors.Is(err, access.ErrNotFound):
		id, err := st.CreateUser(name, "", hash)
		if err != nil {
			return err
		}
		if u, err = st.UserByID(id); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if err := st.SetPassword(u.ID, hash, ""); err != nil {
			return err
		}
		if u.Disabled {
			if err := st.UpdateUser(u.ID, u.Name, u.Email, false); err != nil {
				return err
			}
		}
	}
	if err := st.EnsureOwner(u.ID); err != nil {
		return err
	}
	if st.LocalSignInOff() {
		_ = st.SetLocalSignInOff(false)
		fmt.Println("password sign-in was switched off for single sign-on; it is on again")
	}
	_ = st.Record(access.Entry{Actor: "cli", Action: "user.reset-owner", Target: u.Name})
	fmt.Printf("owner %q is ready; sessions of this user were ended\n", u.Name)
	if generated {
		fmt.Println("new password:", pw)
	}
	return nil
}
