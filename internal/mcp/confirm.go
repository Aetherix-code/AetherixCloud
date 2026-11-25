package mcp

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/aetherix/aetherixcloud/internal/auth"
)

// Confirmation is embedded in the input of every destructive tool.
type Confirmation struct {
	Confirm bool `json:"confirm,omitempty" jsonschema:"set to true to actually perform this destructive action; without it the tool only returns a dry-run summary"`
	// ConfirmationToken is required for credentials without the admin scope:
	// it is returned by the dry run and proves the summary was shown.
	ConfirmationToken string `json:"confirmation_token,omitempty" jsonschema:"token returned by the dry run; required when the credential lacks the admin scope"`
}

const confirmTTL = 10 * time.Minute

func (c *call) isAdminScope() bool { return auth.ScopeLevel(c.p.Scopes) >= auth.LevelAdmin }

func (c *call) confirmMAC(tool, target, exp string) string {
	return c.s.cfg.Auth.Signer.MAC("mcp-confirm", strings.Join([]string{tool, c.p.RateKey(), target, exp}, "\x00"))
}

func (c *call) confirmToken(tool, target string, exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return e + "." + c.confirmMAC(tool, target, e)
}

func (c *call) checkConfirmToken(tool, target, tok string) error {
	e, mac, ok := strings.Cut(tok, ".")
	if !ok {
		return errors.New("malformed confirmation_token")
	}
	sec, err := strconv.ParseInt(e, 10, 64)
	if err != nil || time.Now().Unix() > sec {
		return errors.New("confirmation_token expired; run the dry run again")
	}
	if mac != c.confirmMAC(tool, target, e) {
		return errors.New("confirmation_token does not match this action or credential")
	}
	return nil
}

// gate implements the confirmation protocol. When it returns a non-nil map the
// tool must return it as its result WITHOUT performing the action.
//
//   - confirm absent/false: dry-run summary (never executes).
//   - confirm true + admin scope: executes.
//   - confirm true + non-admin scope: executes only with the confirmation_token
//     from a previous dry run of the same target.
func (c *call) gate(tool, target string, in Confirmation, summary func() (map[string]any, error)) (map[string]any, error) {
	dry := func(note string) (map[string]any, error) {
		sum, err := summary()
		if err != nil {
			return nil, err
		}
		out := map[string]any{"dry_run": true, "executed": false, "requires_confirmation": true, "tool": tool, "summary": sum}
		if c.isAdminScope() {
			out["how_to_confirm"] = "call this tool again with the same arguments and confirm=true"
		} else {
			out["confirmation_token"] = c.confirmToken(tool, target, time.Now().Add(confirmTTL))
			out["how_to_confirm"] = "call this tool again with the same arguments, confirm=true and this confirmation_token"
		}
		if note != "" {
			out["note"] = note
		}
		return out, nil
	}
	if !in.Confirm {
		return dry("")
	}
	if !c.isAdminScope() {
		if in.ConfirmationToken == "" {
			return dry("this credential lacks the admin scope: a dry run must be confirmed with its confirmation_token")
		}
		if err := c.checkConfirmToken(tool, target, in.ConfirmationToken); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
