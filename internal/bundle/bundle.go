// Package bundle builds a reproducible, self-contained guest environment archive.
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

func Build(source fs.FS, profile, revision string) ([]byte, error) {
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(revision) {
		return nil, fmt.Errorf("invalid c2j revision")
	}
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
	input := regexp.MustCompile(`inputs\.c2j\.url = "github:colony-2/c2j[^\"]*";`)
	if !input.Match(files["flake.nix"]) {
		return nil, fmt.Errorf("missing c2j build input")
	}
	files["flake.nix"] = input.ReplaceAll(files["flake.nix"], []byte(`inputs.c2j.url = "github:colony-2/c2j/`+revision+`";`))
	names := []string{}
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	files["profile"] = []byte(profile + "\n")
	names = append(names, "profile")
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
