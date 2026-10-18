package i18n

import "testing"

func TestMatch(t *testing.T) {
	for in, want := range map[string]string{"de-DE,de;q=0.9,en;q=0.8": "de", "en-US,en;q=0.9": "en", "fr-FR,fr;q=0.9": "en", "": "en", "fr, de;q=0.5": "de"} {
		if got := Match(in); got != want {
			t.Errorf("Match(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestT(t *testing.T) {
	if T("de", "Port %d nutzt schon ein anderer Server.", 25565) != "Port 25565 nutzt schon ein anderer Server." {
		t.Error("German formatting")
	}
	if T("en", "Port %d nutzt schon ein anderer Server.", 25565) != "Port 25565 is already used by another server." {
		t.Error("English translation")
	}
	if T("en", "unbekannter Text 50%") != "unbekannter Text 50%" {
		t.Error("unknown keys must pass through unformatted")
	}
}
