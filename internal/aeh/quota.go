package aeh

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A provider's usage limit is not a result of the agent: it says nothing about the task.
// A profile names it by exit code (quota_exit_codes) or by the message its CLI prints
// (quota_messages); either makes the launch's error a *quotaError, which uncertainExit
// treats like an infrastructure failure: no ladder step is spent and the step blocks.

// errProviderQuota marks a launch that ended on a provider's usage limit.
var errProviderQuota = errors.New("provider quota reached")

// quotaError says which configured rule matched. It never holds the agent's output,
// which could carry a secret.
type quotaError struct {
	Rule string // "exit code 3" or "message #1"
}

func (e *quotaError) Error() string { return "provider quota signal (" + e.Rule + ")" }
func (e *quotaError) Unwrap() error { return errProviderQuota }

const (
	quotaTail        = 4 << 10 // bytes of stdout and of stderr examined
	quotaMaxMessages = 8
)

// checkQuotaMessages validates a profile's quota_messages.
func checkQuotaMessages(msgs []string) error {
	if len(msgs) > quotaMaxMessages {
		return fmt.Errorf("at most %d quota_messages", quotaMaxMessages)
	}
	for i, m := range msgs {
		t := strings.TrimSpace(m)
		if len(t) < 8 || len(t) > 120 || !utf8.ValidString(t) {
			return fmt.Errorf("quota_messages[%d] must be 8 to 120 bytes of UTF-8 after trimming", i)
		}
		letter := false
		for _, r := range t {
			if !unicode.IsPrint(r) {
				return fmt.Errorf("quota_messages[%d] has a control character", i)
			}
			letter = letter || unicode.IsLetter(r)
		}
		if !letter {
			return fmt.Errorf("quota_messages[%d] has no letter", i)
		}
	}
	return nil
}

// quotaSignal judges a launch that ended normally with exit code code (> 0), from the
// profile's rules and the tails of its stdout (outPath) and stderr (beside it, .err). It
// returns nil when no rule matches.
func quotaSignal(p Profile, code int, outPath string) error {
	if code <= 0 {
		return nil
	}
	for _, c := range p.QuotaExitCodes {
		if c == code {
			return &quotaError{Rule: fmt.Sprintf("exit code %d", code)}
		}
	}
	if len(p.QuotaMessages) == 0 || outPath == "" {
		return nil
	}
	errPath := strings.TrimSuffix(outPath, ".out") + ".err"
	for _, path := range []string{outPath, errPath} {
		text := strings.ToLower(ansiEscape.ReplaceAllString(fileTail(path, quotaTail), ""))
		for i, m := range p.QuotaMessages {
			if strings.Contains(text, strings.ToLower(strings.TrimSpace(m))) {
				return &quotaError{Rule: fmt.Sprintf("message #%d", i+1)}
			}
		}
	}
	return nil
}

// ansiEscape matches terminal escape sequences (CSI and OSC), removed before matching.
var ansiEscape = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\))`)

// fileTail reads up to n bytes from the end of a file; a file that cannot be read gives "".
func fileTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return string(b)
}

// quotaNote words a quota error for the owner: the profile, its provider and the rule
// that matched, never the agent's output. It is "" for any other error.
func quotaNote(cfg Config, profile string, err error) string {
	var q *quotaError
	if !errors.As(err, &q) {
		return ""
	}
	provider := cfg.Profiles[profile].Provider
	if provider == "" {
		provider = "unnamed"
	}
	return fmt.Sprintf("provider quota reached for profile %q (provider %s, %s). Wait for the limit to reset", profile, provider, q.Rule)
}
