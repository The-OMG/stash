package drive

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RcloneRemote holds the drive-relevant fields resolved from an rclone remote.
type RcloneRemote struct {
	Type               string
	Token              string // OAuth token JSON
	ClientID           string
	ClientSecret       string
	ServiceAccountFile string
	TeamDrive          string
	RootFolderID       string
	Scope              string
}

func rcloneConfigPath() string {
	if p := os.Getenv("RCLONE_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "rclone", "rclone.conf")
}

// parseRcloneConfig parses the rclone INI into section -> key -> value.
func parseRcloneConfig(path string) (map[string]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]map[string]string{}
	var cur string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(line[1 : len(line)-1])
			out[cur] = map[string]string{}
			continue
		}
		if cur == "" {
			continue
		}
		if i := strings.Index(line, "="); i != -1 {
			out[cur][strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	return out, sc.Err()
}

// parseAliasRemote parses an alias's "base,k=v,k2=v2:" value into the base
// remote name and any inline parameter overrides.
func parseAliasRemote(s string) (string, map[string]string) {
	params := map[string]string{}
	s = strings.TrimSuffix(s, ":")
	parts := strings.Split(s, ",")
	base := parts[0]
	for _, p := range parts[1:] {
		if i := strings.Index(p, "="); i != -1 {
			params[strings.TrimSpace(p[:i])] = strings.TrimSpace(p[i+1:])
		}
	}
	return base, params
}

// ResolveRcloneRemote resolves an rclone remote name to its drive parameters,
// following one level of alias (the common gdsa pattern of alias -> drive).
func ResolveRcloneRemote(name string) (*RcloneRemote, error) {
	cfg, err := parseRcloneConfig(rcloneConfigPath())
	if err != nil {
		return nil, err
	}
	sec, ok := cfg[name]
	if !ok {
		return nil, fmt.Errorf("rclone remote %q not found", name)
	}

	var teamOverride, rootOverride string
	if sec["type"] == "alias" {
		base, params := parseAliasRemote(sec["remote"])
		teamOverride = params["team_drive"]
		rootOverride = params["root_folder_id"]
		b, ok := cfg[base]
		if !ok {
			return nil, fmt.Errorf("rclone alias %q base remote %q not found", name, base)
		}
		sec = b
	}
	if sec["type"] != "drive" {
		return nil, fmt.Errorf("rclone remote %q is type %q, not drive", name, sec["type"])
	}

	r := &RcloneRemote{
		Type:               "drive",
		Token:              sec["token"],
		ClientID:           sec["client_id"],
		ClientSecret:       sec["client_secret"],
		ServiceAccountFile: sec["service_account_file"],
		TeamDrive:          sec["team_drive"],
		RootFolderID:       sec["root_folder_id"],
		Scope:              sec["scope"],
	}
	if teamOverride != "" {
		r.TeamDrive = teamOverride
	}
	if rootOverride != "" {
		r.RootFolderID = rootOverride
	}
	return r, nil
}

// Authenticator builds an Authenticator (SA pool or OAuth) from the resolved
// rclone remote.
func (r *RcloneRemote) Authenticator(ctx context.Context, scope string) (Authenticator, error) {
	if scope == "" {
		scope = r.Scope
	}
	if r.ServiceAccountFile != "" {
		return NewSAPool(r.ServiceAccountFile, scope)
	}
	if r.Token != "" {
		return NewOAuthSourceFromRcloneToken(ctx, r.Token, r.ClientID, r.ClientSecret, scope)
	}
	return nil, fmt.Errorf("rclone remote has neither token nor service_account_file")
}

// ListRcloneDriveRemotes returns the names of rclone remotes that resolve to a
// drive (directly or via one alias level), for source-picker UIs.
func ListRcloneDriveRemotes() ([]string, error) {
	cfg, err := parseRcloneConfig(rcloneConfigPath())
	if err != nil {
		return nil, err
	}
	var names []string
	for name, sec := range cfg {
		switch sec["type"] {
		case "drive":
			names = append(names, name)
		case "alias":
			base, _ := parseAliasRemote(sec["remote"])
			if b, ok := cfg[base]; ok && b["type"] == "drive" {
				names = append(names, name)
			}
		}
	}
	return names, nil
}
