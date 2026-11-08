package access

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1, secret "12345678901234567890"; the RFC lists eight digits, the last six are used here.
func TestTOTPMatchesTheRFCVectors(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037", 20000000000: "353130"} {
		if got := totpAt(key, unix/30); got != want {
			t.Errorf("t=%d: %s, want %s", unix, got, want)
		}
	}
}

func TestTOTPWindowAndReplay(t *testing.T) {
	secret := NewTOTPSecret()
	key, err := totpKey(secret)
	if err != nil || len(key) != 20 {
		t.Fatalf("secret: %v %d", err, len(key))
	}
	now := time.Unix(1_700_000_000, 0)
	step := now.Unix() / 30
	if _, ok := checkTOTP(secret, totpAt(key, step), now, 0); !ok {
		t.Fatal("the current code must work")
	}
	if _, ok := checkTOTP(secret, totpAt(key, step-1), now, 0); !ok {
		t.Fatal("one step of clock drift is allowed")
	}
	if _, ok := checkTOTP(secret, totpAt(key, step+1), now, 0); !ok {
		t.Fatal("one step ahead is allowed")
	}
	if _, ok := checkTOTP(secret, totpAt(key, step-3), now, 0); ok {
		t.Fatal("an old code must not work")
	}
	if _, ok := checkTOTP(secret, totpAt(key, step), now, step); ok {
		t.Fatal("a step that was used already must be refused")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "  "} {
		if _, ok := checkTOTP(secret, bad, now, 0); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, ok := checkTOTP(secret, " "+totpAt(key, step)[:3]+" "+totpAt(key, step)[3:], now, 0); !ok {
		t.Fatal("spaces in a typed code are fine")
	}
}

func TestTOTPURI(t *testing.T) {
	u := TOTPURI("Cubeherd", "anna@example.com", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{"otpauth://totp/Cubeherd:anna@example.com?", "secret=JBSWY3DPEHPK3PXP", "issuer=Cubeherd", "digits=6", "period=30"} {
		if !strings.Contains(strings.ReplaceAll(u, "%40", "@"), want) {
			t.Errorf("%q missing in %s", want, u)
		}
	}
}

func TestEnableVerifyAndRecover(t *testing.T) {
	s := open(t)
	u := user(t, s, "tina")
	secret := NewTOTPSecret()
	key, _ := totpKey(secret)
	now := time.Now()
	if _, err := s.EnableTOTP(u.ID, secret, "000000", now); err == nil {
		t.Fatal("a wrong code must not enable two-factor")
	}
	codes, err := s.EnableTOTP(u.ID, secret, totpAt(key, now.Unix()/30), now)
	if err != nil || len(codes) != 10 {
		t.Fatalf("enable: %v %v", err, codes)
	}
	if got, _ := s.UserByID(u.ID); !got.TOTP {
		t.Fatal("not enabled")
	}
	raw, _ := s.db.Query("SELECT totp_secret FROM users WHERE id=?", u.ID)
	var stored string
	raw.Next()
	raw.Scan(&stored)
	raw.Close()
	if stored == secret || stored == "" {
		t.Fatal("the secret must be stored encrypted")
	}
	// the code used for setup is spent; the next step's code works once
	next := totpAt(key, now.Unix()/30+1)
	if s.VerifyTOTP(u.ID, totpAt(key, now.Unix()/30), now) {
		t.Fatal("the setup code was reused")
	}
	if !s.VerifyTOTP(u.ID, next, now) || s.VerifyTOTP(u.ID, next, now) {
		t.Fatal("a code works once")
	}
	// recovery codes: single use, formatted, case and dashes forgiven
	if !strings.Contains(codes[0], "-") || len(codes[0]) != 11 {
		t.Fatalf("format %q", codes[0])
	}
	if !s.UseRecoveryCode(u.ID, strings.ToUpper(codes[0])) || s.UseRecoveryCode(u.ID, codes[0]) {
		t.Fatal("a recovery code works once")
	}
	if s.UseRecoveryCode(u.ID, "aaaaa-aaaaa") || s.RecoveryCodesLeft(u.ID) != 9 {
		t.Fatalf("left %d", s.RecoveryCodesLeft(u.ID))
	}
	var hashes int
	s.db.QueryRow("SELECT COUNT(*) FROM recovery_codes WHERE hash IN (?,?)", codes[1], strings.ReplaceAll(codes[1], "-", "")).Scan(&hashes)
	if hashes != 0 {
		t.Fatal("recovery codes must be stored as hashes")
	}
	fresh, _ := s.NewRecoveryCodes(u.ID)
	if s.UseRecoveryCode(u.ID, codes[2]) || len(fresh) != 10 {
		t.Fatal("new codes replace the old ones")
	}
	if err := s.DisableTOTP(u.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByID(u.ID); got.TOTP || s.RecoveryCodesLeft(u.ID) != 0 || s.VerifyTOTP(u.ID, totpAt(key, now.Unix()/30+2), now) {
		t.Fatal("disabling must remove the second factor")
	}
	_ = base32.StdEncoding
}

func TestTokensAreSingleUseAndExpire(t *testing.T) {
	s := open(t)
	u := user(t, s, "toni")
	tok, err := s.NewToken(u.ID, TokenReset, time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if raw := s.KV("x"); raw != "" {
		t.Fatal()
	}
	var stored string
	s.db.QueryRow("SELECT id_hash FROM tokens").Scan(&stored)
	if stored == tok || len(stored) != 64 {
		t.Fatal("only a hash of the token may be stored")
	}
	if _, err := s.PeekToken(tok, TokenVerify); err == nil {
		t.Fatal("a token is only good for its own kind")
	}
	if got, err := s.PeekToken(tok, TokenReset); err != nil || got.UserID != u.ID {
		t.Fatalf("peek: %v", err)
	}
	if _, err := s.ConsumeToken(tok, TokenReset); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeToken(tok, TokenReset); err == nil {
		t.Fatal("a token works once")
	}
	if _, err := s.PeekToken("made-up", TokenReset); err == nil {
		t.Fatal("unknown token")
	}
	exp, _ := s.NewToken(u.ID, TokenVerify, -time.Minute, "")
	if _, err := s.ConsumeToken(exp, TokenVerify); err == nil {
		t.Fatal("an expired token must fail")
	}
	// a new reset token replaces the earlier one
	a, _ := s.NewToken(u.ID, TokenReset, time.Hour, "")
	b, _ := s.NewToken(u.ID, TokenReset, time.Hour, "")
	if _, err := s.PeekToken(a, TokenReset); err == nil {
		t.Fatal("asking again must invalidate the earlier link")
	}
	if _, err := s.PeekToken(b, TokenReset); err != nil {
		t.Fatal(err)
	}
	// a token dies with its user
	_ = s.DeleteUser(u.ID)
	if _, err := s.PeekToken(b, TokenReset); err == nil {
		t.Fatal("tokens of a deleted user must go")
	}
}

func TestConcurrentConsumeHasOneWinner(t *testing.T) {
	s := open(t)
	u := user(t, s, "cora")
	tok, _ := s.NewToken(u.ID, TokenReset, time.Hour, "")
	wins := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		go func() { _, err := s.ConsumeToken(tok, TokenReset); wins <- err == nil }()
	}
	n := 0
	for i := 0; i < 8; i++ {
		if <-wins {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d callers got the token", n)
	}
}

func TestDevices(t *testing.T) {
	s := open(t)
	u := user(t, s, "dora")
	if s.SeenDevice(u.ID, "", "1.1.1.1", "ua") {
		t.Fatal("no device cookie means unknown")
	}
	if s.SeenDevice(u.ID, "dev-1", "1.1.1.1", "ua") {
		t.Fatal("the first sign-in is from a new device")
	}
	if !s.SeenDevice(u.ID, "dev-1", "2.2.2.2", "ua") {
		t.Fatal("the same device must be recognised even from another address")
	}
	if s.SeenDevice(u.ID, "dev-2", "1.1.1.1", "ua") {
		t.Fatal("another device is new")
	}
	other := user(t, s, "olaf")
	if s.SeenDevice(other.ID, "dev-1", "1.1.1.1", "ua") {
		t.Fatal("devices belong to one user")
	}
}

func TestSecuritySettings(t *testing.T) {
	s := open(t)
	d := s.Security()
	if d.Registration != RegClosed || d.Notice != "off" || d.TwoFA != "off" || d.GraceDays != 7 {
		t.Fatalf("defaults: %+v", d)
	}
	if err := s.SaveSecurity(Security{Registration: RegApproval, Notice: "new", TwoFA: "privileged", GraceDays: 3}); err != nil {
		t.Fatal(err)
	}
	v := s.Security()
	if v.Registration != RegApproval || v.TwoFASince == 0 || v.GraceDays != 3 {
		t.Fatalf("saved: %+v", v)
	}
	since := v.TwoFASince
	v.Notice = "every"
	_ = s.SaveSecurity(v)
	if s.Security().TwoFASince != since {
		t.Fatal("changing something else must not restart the grace period")
	}
	v.TwoFA = "off"
	_ = s.SaveSecurity(v)
	if s.Security().TwoFASince != 0 {
		t.Fatal("switching two-factor off clears the clock")
	}
}

func TestPendingAccountsAndForcedResets(t *testing.T) {
	s := open(t)
	id, err := s.CreateUserWith(NewUser{Name: "paula", Email: "paula@example.com", Unverified: true, Status: StatusPending})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.UserByID(id)
	if u.Status != StatusPending || u.EmailVerified {
		t.Fatalf("%+v", u)
	}
	if p, _ := s.PendingUsers(); len(p) != 1 || p[0].ID != id {
		t.Fatalf("pending: %v", p)
	}
	_ = s.SetStatus(id, StatusActive)
	if p, _ := s.PendingUsers(); len(p) != 0 {
		t.Fatal("an approved account leaves the list")
	}
	_ = s.SetForceReset(id, true)
	if u, _ := s.UserByID(id); !u.ForceReset {
		t.Fatal("not flagged")
	}
	_ = s.SetPassword(id, "hash", "")
	if u, _ := s.UserByID(id); u.ForceReset {
		t.Fatal("a new password clears the flag")
	}
	other, _ := s.CreateUser("quentin", "", "")
	if u, _ := s.UserByID(other); u.Status != StatusActive || !u.EmailVerified {
		t.Fatal("accounts made by an admin are active and confirmed")
	}
}

func TestSessionMethod(t *testing.T) {
	s := open(t)
	u := user(t, s, "sven")
	a, _ := s.NewSession(u.ID, "", "", time.Hour)
	b, _ := s.NewSessionVia(u.ID, "", "", time.Hour, "sso")
	if s.SessionMethod(a) != "password" || s.SessionMethod(b) != "sso" {
		t.Fatalf("%q %q", s.SessionMethod(a), s.SessionMethod(b))
	}
}

func TestMigrationGivesCustomRolesTheSplitSettingsRights(t *testing.T) {
	s := open(t)
	id, err := s.SaveRole(Role{Name: "Custom", Perms: []Perm{ServerView, ServerSettings}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SaveRole(Role{Name: "Watcher", Perms: []Perm{ServerView}})
	if err != nil {
		t.Fatal(err)
	}
	// SaveRole already stores what is given; remove what the migration must add, then run it again
	if _, err := s.db.Exec("DELETE FROM role_perms WHERE perm IN ('server.resources','server.network','server.advanced')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(migrations[3]); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.ListRoles()
	for _, r := range rs {
		has := func(p Perm) bool {
			for _, x := range r.Perms {
				if x == p {
					return true
				}
			}
			return false
		}
		switch r.ID {
		case id:
			if !has(ServerResources) || !has(ServerNetwork) || !has(ServerAdvanced) {
				t.Errorf("a role that could change settings lost something: %v", r.Perms)
			}
		case other:
			if has(ServerResources) || has(ServerNetwork) || has(ServerAdvanced) {
				t.Errorf("a role without settings gained rights: %v", r.Perms)
			}
		}
	}
}

func TestSuspendIsForOwnersAndAdminsOnly(t *testing.T) {
	s := open(t)
	rs, _ := s.ListRoles()
	for _, r := range rs {
		has := false
		for _, p := range r.Perms {
			has = has || p == ServerSuspend
		}
		want := r.Name == RoleOwner || r.Name == RoleAdmin
		if has != want {
			t.Errorf("%s: server.suspend = %v, want %v", r.Name, has, want)
		}
	}
}
