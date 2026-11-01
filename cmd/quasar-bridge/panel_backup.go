package main

import (
	stdjson "encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// secretName reports whether a member name looks like it holds a credential.
// The config is not meant to hold keys at all (they live in secrets.json), so
// this is a second lock, not the first. Counters such as max_tokens stay.
func secretName(name string) bool {
	name = strings.ToLower(name)
	if name == "key" {
		return true
	}
	for _, part := range []string{"secret", "password", "passwd", "authorization", "apikey", "api_key", "cookie"} {
		if strings.Contains(name, part) {
			return true
		}
	}
	return strings.Contains(name, "token") && !strings.Contains(name, "max") && !strings.HasSuffix(name, "tokens")
}
// scrubSecrets blanks the non-empty secret members of a decoded JSON value in
// place and reports how many it blanked.
func scrubSecrets(value any) int {
	blanked := 0
	switch v := value.(type) {
	case map[string]any:
		for name, member := range v {
			if text, ok := member.(string); ok && text != "" && secretName(name) {
				v[name] = ""
				blanked++
				continue
			}
			blanked += scrubSecrets(member)
		}
	case []any:
		for _, item := range v {
			blanked += scrubSecrets(item)
		}
	}
	return blanked
}

// exportSettings returns the config file as indented JSON with secrets blanked.
func exportSettings(configPath string) ([]byte, int, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, 0, err
	}
	var doc any
	if err := stdjson.Unmarshal(raw, &doc); err != nil {
		return nil, 0, err
	}
	blanked := scrubSecrets(doc)
	out, err := stdjson.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, 0, err
	}
	return append(out, '\n'), blanked, nil
}

// apiBackup hands the operator a copy of the settings to keep before an
// update. It is a download, not a restore: putting a file back stays a
// deliberate act on the station.
func (p *panelServer) apiBackup(w http.ResponseWriter, r *http.Request) {
	data, blanked, err := exportSettings(p.configPath)
	if err != nil {
		p.audit.record("backup", r, "failed", nil)
		http.Error(w, "settings are not readable", http.StatusInternalServerError)
		return
	}
	p.audit.record("backup", r, "ok", map[string]any{"blanked": blanked})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="midi-settings-`+time.Now().Format("20060102-150405")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
