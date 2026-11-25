package mcp

import (
	"regexp"
	"strconv"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"google.golang.org/protobuf/proto"
)

// Redacted replaces secret values in everything returned to agents.
const Redacted = "[REDACTED]"

var (
	secretKeyRe = regexp.MustCompile(`(?i)(pass(word|wd)?|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|auth|cert|dsn|salt|signing|seed)`)
	redactRes   = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
		regexp.MustCompile(`\baxt_[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`\baxr_[A-Za-z0-9_.-]{16,}`),
		regexp.MustCompile(`\baxjoin1\.[A-Za-z0-9_.-]{16,}`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`),
	}
	userinfoRe = regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+(@)`)
	kvRe       = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|credential)[A-Za-z0-9_.-]*)(["']?\s*[:=]\s*["']?)([^\s"',;&]+)`)
)

// RedactText scrubs credentials from free text (logs, build output).
func RedactText(s string) string {
	for _, re := range redactRes {
		s = re.ReplaceAllString(s, Redacted)
	}
	s = userinfoRe.ReplaceAllString(s, "${1}"+Redacted+"${2}")
	s = kvRe.ReplaceAllString(s, "${1}${2}"+Redacted)
	return s
}

// redactEnv redacts env values whose key looks secret and scrubs the rest.
func redactEnv(env map[string]string) {
	for k, v := range env {
		if secretKeyRe.MatchString(k) {
			env[k] = Redacted
		} else {
			env[k] = RedactText(v)
		}
	}
}

func redactSpec(sp *v1.ServiceSpec) {
	if sp == nil {
		return
	}
	redactEnv(sp.Env)
	redactEnv(sp.Labels)
	if sp.Build != nil {
		redactEnv(sp.Build.Args)
	}
	for i, c := range sp.Command {
		sp.Command[i] = RedactText(c)
	}
}

// redactedService returns a redacted deep copy.
func redactedService(s *v1.Service) *v1.Service {
	c := proto.Clone(s).(*v1.Service)
	redactSpec(c.Spec)
	return c
}

func redactedDeployment(d *v1.Deployment) *v1.Deployment {
	c := proto.Clone(d).(*v1.Deployment)
	redactSpec(c.Spec)
	c.StatusMessage = RedactText(c.StatusMessage)
	return c
}

// redactedVariable never exposes secret values, whatever the API returned.
func redactedVariable(v *v1.Variable) *v1.Variable {
	c := proto.Clone(v).(*v1.Variable)
	if c.Secret || c.Redacted || secretKeyRe.MatchString(c.Key) {
		c.Value, c.Redacted = Redacted, true
	} else {
		c.Value = RedactText(c.Value)
	}
	return c
}

var sensitiveArg = regexp.MustCompile(`(?i)(pass|secret|token|key|yaml|plaintext|value)`)

// auditAttrs flattens tool arguments for the audit log: scalars only, no
// sensitive-looking names, maps reduced to their key names.
func auditAttrs(args map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range args {
		switch x := v.(type) {
		case map[string]any:
			names := make([]string, 0, len(x))
			for n := range x {
				names = append(names, n)
			}
			out[k+"_keys"] = strings.Join(names, ",")
			continue
		case string, bool, float64:
		default:
			continue
		}
		if sensitiveArg.MatchString(k) {
			continue
		}
		s := ""
		switch x := v.(type) {
		case string:
			s = x
		case bool:
			if x {
				s = "true"
			} else {
				s = "false"
			}
		case float64:
			s = formatFloat(x)
		}
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		out[k] = s
	}
	return out
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
