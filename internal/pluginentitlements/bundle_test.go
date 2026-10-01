//go:build linux || darwin

package pluginentitlements

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBundleJSONStrictTransport(t *testing.T) {
	encoded := base64.RawURLEncoding.EncodeToString([]byte{0xd2, 0x80})
	raw, err := json.Marshal(map[string]any{"schema_version": 1, "grants": []string{encoded}, "revocations": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := ParseBundleJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Grants) != 1 || !bytes.Equal(bundle.Grants[0], []byte{0xd2, 0x80}) || len(bundle.Revocations) != 0 || bundle.Digest == [32]byte{} {
		t.Fatalf("wrong transport projection: %+v", bundle)
	}
	for name, candidate := range map[string][]byte{
		"duplicate key":    []byte(`{"schema_version":1,"grants":[],"grants":[],"revocations":[]}`),
		"unknown key":      []byte(`{"schema_version":1,"grants":[],"revocations":[],"extra":1}`),
		"wrong field case": []byte(`{"Schema_Version":1,"grants":[],"revocations":[]}`),
		"null array":       []byte(`{"schema_version":1,"grants":null,"revocations":[]}`),
		"padded base64":    []byte(`{"schema_version":1,"grants":["0oA="],"revocations":[]}`),
		"trailing JSON":    append(bytes.Clone(raw), []byte(` {}`)...),
		"invalid UTF-8":    append(bytes.Clone(raw), 0xff),
		"oversize":         []byte(strings.Repeat(" ", MaxBundleBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBundleJSON(candidate); err == nil {
				t.Fatal("accepted invalid bundle transport")
			}
		})
	}
}

func TestReadRegularFileForbidsSymlinksAndNonFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(realDir, "bundle.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"grants":[],"revocations":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if raw, err := readRegularFile(path, 1024); err != nil || len(raw) == 0 {
		t.Fatalf("regular file failed: %v", err)
	}
	fileLink := filepath.Join(root, "file-link")
	if err := os.Symlink(path, fileLink); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(root, "dir-link")
	if err := os.Symlink(realDir, dirLink); err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]string{
		"relative": "bundle.json", "final symlink": fileLink,
		"parent symlink": filepath.Join(dirLink, "bundle.json"), "directory": realDir,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readRegularFile(candidate, 1024); err == nil {
				t.Fatal("accepted unsafe file path")
			}
		})
	}
	if _, err := readRegularFile(path, 4); err == nil {
		t.Fatal("accepted over-limit file")
	}
	empty := filepath.Join(realDir, "empty")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularFile(empty, 1024); err == nil {
		t.Fatal("accepted empty file")
	}
}
