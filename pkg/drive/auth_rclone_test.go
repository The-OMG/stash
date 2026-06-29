package drive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeScope(t *testing.T) {
	cases := map[string]string{
		"":                 DefaultScope,
		"  ":               DefaultScope,
		"drive.readonly":   "https://www.googleapis.com/auth/drive.readonly",
		"drive":            "https://www.googleapis.com/auth/drive",
		"https://example/x": "https://example/x", // already a URL, left alone
	}
	for in, want := range cases {
		if got := normalizeScope(in); got != want {
			t.Errorf("normalizeScope(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestParseRcloneConfig(t *testing.T) {
	cfg := "[mydrive]\n" +
		"type = drive\n" +
		"scope = drive\n" +
		"team_drive = 0ABCdef\n" +
		"token = {\"access_token\":\"x\",\"refresh_token\":\"y\"}\n" +
		"\n" +
		"[s3thing]\n" +
		"type = s3\n"

	path := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	remotes, err := parseRcloneConfig(path)
	if err != nil {
		t.Fatalf("parseRcloneConfig: %v", err)
	}
	md, ok := remotes["mydrive"]
	if !ok {
		t.Fatalf("missing [mydrive]; got %v", keysOf(remotes))
	}
	if md["type"] != "drive" || md["team_drive"] != "0ABCdef" || md["scope"] != "drive" {
		t.Errorf("mydrive parsed wrong: %+v", md)
	}
	if md["token"] != `{"access_token":"x","refresh_token":"y"}` {
		t.Errorf("token value not preserved: %q", md["token"])
	}
	if remotes["s3thing"]["type"] != "s3" {
		t.Errorf("s3thing type = %q; want s3", remotes["s3thing"]["type"])
	}
}

func keysOf(m map[string]map[string]string) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	return k
}
