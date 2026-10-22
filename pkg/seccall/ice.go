package seccall

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// DefaultSTUN is used when no STUN servers are saved.
var DefaultSTUN = []string{"stun:stun.l.google.com:19302", "stun:stun1.l.google.com:19302"}

// ICEServer is one entry of RTCConfiguration.iceServers.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ICEResult is what a joined participant gets from GET …/ice.
type ICEResult struct {
	ICEServers []ICEServer `json:"ice_servers"`
	Relay      bool        `json:"relay"` // a TURN relay is configured
	Mode       string      `json:"mode"`
	ExpiresAt  time.Time   `json:"expires_at"`
}

// SettingsView is the super admin's view (secrets are never returned).
type SettingsView struct {
	Mode          string    `json:"mode"`
	STUNURLs      []string  `json:"stun_urls"`
	TURNURLs      []string  `json:"turn_urls"`
	Username      string    `json:"username"`
	CredentialSet bool      `json:"credential_set"`
	CredentialHnt string    `json:"credential_hint"`
	SecretSet     bool      `json:"shared_secret_set"`
	UpdatedAt     time.Time `json:"updated_at"`
	UpdatedBy     string    `json:"updated_by"`
	DefaultSTUN   []string  `json:"default_stun_urls"`
}

// SettingsInput is the body of PUT /secure-call-settings. Credential and
// SharedSecret are write-only: empty keeps the saved value; ClearX removes it.
type SettingsInput struct {
	Mode            string   `json:"mode"`
	STUNURLs        []string `json:"stun_urls"`
	TURNURLs        []string `json:"turn_urls"`
	Username        string   `json:"username"`
	Credential      string   `json:"credential"`
	SharedSecret    string   `json:"shared_secret"`
	ClearCredential bool     `json:"clear_credential"`
	ClearSecret     bool     `json:"clear_shared_secret"`
}

func splitList(s string) []string {
	out := []string{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == ';' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func cleanURLs(in []string, schemes ...string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range in {
		for _, u := range splitList(raw) {
			ok := false
			for _, sc := range schemes {
				if strings.HasPrefix(strings.ToLower(u), sc+":") {
					ok = true
				}
			}
			if !ok {
				return nil, fmt.Errorf("%q must start with %s", u, strings.Join(schemes, ": or ")+":")
			}
			if len(u) > 300 {
				return nil, fmt.Errorf("%q is too long", u)
			}
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	if len(out) > 10 {
		return nil, errors.New("at most 10 servers")
	}
	return out, nil
}

// LoadSettings returns the saved settings (defaults when none are saved).
func (s *Service) LoadSettings() Settings {
	var st Settings
	if res := s.DB.Where("id = ?", "platform").Limit(1).Find(&st); res.Error != nil || res.RowsAffected == 0 {
		return Settings{ID: "platform", Mode: ModeSTUN}
	}
	if st.Mode == "" {
		st.Mode = ModeSTUN
	}
	return st
}

// SettingsFor renders settings for the super admin.
func (s *Service) SettingsFor() SettingsView {
	st := s.LoadSettings()
	v := SettingsView{Mode: st.Mode, STUNURLs: splitList(st.STUNURLs), TURNURLs: splitList(st.TURNURLs), Username: st.Username,
		CredentialSet: st.Credential != "", SecretSet: st.SharedSecret != "", UpdatedAt: st.UpdatedAt, UpdatedBy: st.UpdatedBy, DefaultSTUN: DefaultSTUN}
	if st.Credential != "" {
		v.CredentialHnt = "••••"
		if pw, err := mail.DecryptSecret(st.Credential); err == nil && len(pw) > 8 {
			v.CredentialHnt = "••••" + pw[len(pw)-2:]
		}
	}
	return v
}

// SaveSettings validates and stores the ICE settings (secrets encrypted).
func (s *Service) SaveSettings(in SettingsInput, by string) (SettingsView, error) {
	st := s.LoadSettings()
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = ModeSTUN
	}
	if mode != ModeSTUN && mode != ModeStatic && mode != ModeCoturn {
		return SettingsView{}, invalid("mode must be stun, static or coturn")
	}
	stun, err := cleanURLs(in.STUNURLs, "stun", "stuns")
	if err != nil {
		return SettingsView{}, invalid("STUN servers: " + err.Error())
	}
	turn, err := cleanURLs(in.TURNURLs, "turn", "turns")
	if err != nil {
		return SettingsView{}, invalid("TURN servers: " + err.Error())
	}
	st.ID, st.Mode = "platform", mode
	st.STUNURLs, st.TURNURLs = strings.Join(stun, "\n"), strings.Join(turn, "\n")
	st.Username = strings.TrimSpace(in.Username)
	if in.ClearCredential {
		st.Credential = ""
	}
	if c := strings.TrimSpace(in.Credential); c != "" {
		enc, err := mail.EncryptSecret(c)
		if err != nil {
			return SettingsView{}, err
		}
		st.Credential = enc
	}
	if in.ClearSecret {
		st.SharedSecret = ""
	}
	if c := strings.TrimSpace(in.SharedSecret); c != "" {
		if len(c) < 16 {
			return SettingsView{}, invalid("The shared secret should be at least 16 characters (use the static-auth-secret from turnserver.conf).")
		}
		enc, err := mail.EncryptSecret(c)
		if err != nil {
			return SettingsView{}, err
		}
		st.SharedSecret = enc
	}
	switch mode {
	case ModeStatic:
		if len(turn) == 0 || st.Username == "" || st.Credential == "" {
			return SettingsView{}, invalid("A TURN server with a fixed login needs at least one turn: URL, a username and a password.")
		}
	case ModeCoturn:
		if len(turn) == 0 || st.SharedSecret == "" {
			return SettingsView{}, invalid("coturn with a shared secret needs at least one turn: URL and the shared secret.")
		}
	}
	st.UpdatedAt, st.UpdatedBy = s.now(), by
	if err := s.DB.Save(&st).Error; err != nil {
		return SettingsView{}, err
	}
	return s.SettingsFor(), nil
}

// CoturnCredential is the TURN REST API credential (coturn use-auth-secret):
// username "<expiry unix>:<id>", password base64(HMAC-SHA1(secret, username)).
func CoturnCredential(secret, id string, expires time.Time) (username, credential string) {
	username = fmt.Sprintf("%d:%s", expires.Unix(), id)
	m := hmac.New(sha1.New, []byte(secret))
	m.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// iceServers builds the list for one participant (id) until expires.
func (s *Service) iceServers(id string, expires time.Time) (ICEResult, error) {
	st := s.LoadSettings()
	stun := splitList(st.STUNURLs)
	if len(stun) == 0 {
		stun = DefaultSTUN
	}
	out := ICEResult{ICEServers: []ICEServer{{URLs: stun}}, Mode: st.Mode, ExpiresAt: expires}
	turn := splitList(st.TURNURLs)
	switch st.Mode {
	case ModeStatic:
		pw, err := mail.DecryptSecret(st.Credential)
		if err != nil || len(turn) == 0 {
			return out, nil // misconfigured: STUN only
		}
		out.ICEServers = append(out.ICEServers, ICEServer{URLs: turn, Username: st.Username, Credential: pw})
		out.Relay = true
	case ModeCoturn:
		secret, err := mail.DecryptSecret(st.SharedSecret)
		if err != nil || secret == "" || len(turn) == 0 {
			return out, nil
		}
		u, c := CoturnCredential(secret, id, expires)
		out.ICEServers = append(out.ICEServers, ICEServer{URLs: turn, Username: u, Credential: c})
		out.Relay = true
	}
	return out, nil
}

// ICE returns the ICE servers for a joined participant (TURN credentials, in
// coturn mode, are valid until the call expires).
func (s *Service) ICE(roomID, token string) (*ICEResult, error) {
	p, err := s.participant(s.DB, roomID, token)
	if err != nil {
		return nil, err
	}
	if err := stateErr(p); err != nil {
		return nil, err
	}
	if p.State == StateLobby {
		return nil, errWaiting
	}
	r, err := s.room(roomID)
	if err != nil {
		return nil, err
	}
	if s.status(r) == "ended" {
		return nil, errEnded
	}
	res, err := s.iceServers(p.ID, r.ExpiresAt)
	if err != nil {
		return nil, refuse(http.StatusInternalServerError, "ICE", err.Error())
	}
	return &res, nil
}

// PreviewICE is what a participant would get, for the super admin's
// "Test from this browser" (credentials valid for 10 minutes).
func (s *Service) PreviewICE() ICEResult {
	res, _ := s.iceServers("settings-test", s.now().Add(10*time.Minute))
	return res
}
