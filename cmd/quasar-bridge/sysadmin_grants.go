package main

import (
	"errors"
	"regexp"

	"quasarbridge/internal/sysadmin"
)

// maxSessionGrants bounds the in-memory session permissions.
const maxSessionGrants = 200

func sessionKey(tool, host, subject string) string {
	return tool + "\x00" + host + "\x00" + subject
}

// SessionAllowed reports whether the operator approved exactly this action for
// the current bridge session. The permission is lost on restart.
func (h *sysadminHub) SessionAllowed(tool, host, subject string) bool {
	if tool == "" || subject == "" {
		return false
	}
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	_, ok := h.session[sessionKey(tool, host, subject)]
	return ok
}

// SessionGrants returns the number of remembered session permissions.
func (h *sysadminHub) SessionGrants() int {
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	return len(h.session)
}

// ClearSession drops all session permissions.
func (h *sysadminHub) ClearSession() {
	h.sessMu.Lock()
	h.session = nil
	h.sessMu.Unlock()
}

// grant is the queue's granter: it stores a session or an always permission
// for exactly the approved tool, host and subject. Only a human decision
// reaches it, and never for forbidden or sensitive actions (those carry no
// subject).
func (h *sysadminHub) grant(it sysadmin.Item, scope sysadmin.Scope) error {
	tool, subject := it.PolicyTool(), it.Subject()
	if tool == "" || subject == "" {
		return errors.New("нечего запоминать")
	}
	switch scope {
	case sysadmin.ScopeSession:
		h.sessMu.Lock()
		defer h.sessMu.Unlock()
		if h.session == nil {
			h.session = map[string]struct{}{}
		}
		key := sessionKey(tool, it.Host, subject)
		if _, ok := h.session[key]; !ok && len(h.session) >= maxSessionGrants {
			return errors.New("слишком много разрешений сессии")
		}
		h.session[key] = struct{}{}
		return nil
	case sysadmin.ScopeAlways:
		return h.addAllowRule(tool, "re:^"+regexp.QuoteMeta(subject)+"$")
	}
	return errors.New("неизвестная область")
}

// addAllowRule puts an allow rule in front of the stored rules, so an explicit
// "always" beats an earlier ask rule for the same subject. Duplicates are
// skipped. A deny rule never gets here: denied actions are not queued.
func (h *sysadminHub) addAllowRule(tool, match string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.loadLocked(); err != nil {
		return err
	}
	for _, r := range h.cfg.Policy.Rules {
		if r.Tool == tool && r.Match == match && r.Mode == string(sysadmin.ModeAllow) {
			return nil
		}
	}
	if _, err := regexp.Compile(match[len("re:"):]); err != nil {
		return err
	}
	rule := sysadminRuleCfg{Tool: tool, Match: match, Mode: string(sysadmin.ModeAllow)}
	h.cfg.Policy.Rules = append([]sysadminRuleCfg{rule}, h.cfg.Policy.Rules...)
	if h.cfg.Policy.Default == "" {
		h.cfg.Policy.Default = string(sysadmin.ModeAsk)
	}
	return h.saveCfgLocked()
}
