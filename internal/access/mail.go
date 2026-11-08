package access

import (
	"encoding/json"

	"cubeherd/internal/mailer"
)

type mailStored struct {
	mailer.Config
	PasswordEnc string `json:"password,omitempty"`
}

// MailConfig returns the SMTP settings with the password decrypted; the zero Config means none are set.
func (s *Store) MailConfig() (mailer.Config, error) {
	raw := s.KV("mail.smtp")
	if raw == "" {
		return mailer.Config{}, nil
	}
	var m mailStored
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return mailer.Config{}, err
	}
	pw, err := s.open(m.PasswordEnc)
	if err != nil {
		return mailer.Config{}, err
	}
	m.Config.Password = pw
	return m.Config, nil
}

// SaveMailConfig stores the SMTP settings; the password is encrypted with the installation's key.
func (s *Store) SaveMailConfig(c mailer.Config) error {
	enc, err := s.seal(c.Password)
	if err != nil {
		return err
	}
	b, err := json.Marshal(mailStored{Config: c, PasswordEnc: enc})
	if err != nil {
		return err
	}
	return s.SetKV("mail.smtp", string(b))
}
