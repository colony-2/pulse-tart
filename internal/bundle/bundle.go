// Package bundle builds a reproducible, self-contained guest environment archive.
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

func Build(source fs.FS, profile string) ([]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(source, ".", func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := fs.ReadFile(source, path)
		if e != nil {
			return e
		}
		name := strings.TrimPrefix(path, "scripts/")
		files[name] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	names := []string{}
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	fmt.Fprintln(h, profile)
	for _, n := range names {
		if n == "flake.nix" || n == "flake.lock" || n == "install-guest.sh" || strings.HasPrefix(n, "guest/") || (profile == "integration" && strings.HasPrefix(n, "tests/")) {
			fmt.Fprintf(h, "%s\x00%d\x00", n, len(files[n]))
			h.Write(files[n])
		}
	}
	files["environment.sha256"] = []byte(hex.EncodeToString(h.Sum(nil)) + "\n")
	files["profile"] = []byte(profile + "\n")
	names = append(names, "environment.sha256", "profile")
	sort.Strings(names)
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		b := files[n]
		if e := tw.WriteHeader(&tar.Header{Name: n, Mode: 0644, Size: int64(len(b))}); e != nil {
			return nil, e
		}
		if _, e := tw.Write(b); e != nil {
			return nil, e
		}
	}
	if e := tw.Close(); e != nil {
		return nil, e
	}
	if e := gz.Close(); e != nil {
		return nil, e
	}
	return out.Bytes(), nil
}
