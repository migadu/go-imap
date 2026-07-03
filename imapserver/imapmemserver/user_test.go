package imapmemserver

import (
	"context"
	"testing"
)

// I1: Login must reject unknown users and wrong passwords, and accept the valid
// pair. The comparison no longer short-circuits on the username (to avoid a
// timing side channel); this locks in that the behavior is still correct.
func TestUserLoginRejectsUnknownUserAndWrongPassword(t *testing.T) {
	u := NewUser("alice", "secret")
	ctx := context.Background()

	if err := u.Login(ctx, "alice", "secret"); err != nil {
		t.Errorf("valid login = %v, want nil", err)
	}
	if err := u.Login(ctx, "bob", "secret"); err == nil {
		t.Error("unknown user should fail")
	}
	if err := u.Login(ctx, "alice", "wrong"); err == nil {
		t.Error("wrong password should fail")
	}
	if err := u.Login(ctx, "bob", "wrong"); err == nil {
		t.Error("unknown user + wrong password should fail")
	}
}
