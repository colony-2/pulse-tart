// Package releases resolves c2j only when preparing a missing environment.
package releases

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var commit = regexp.MustCompile(`^[a-f0-9]{40}$`)
var selector = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func Resolve(ctx context.Context, version string) (string, error) {
	return resolve(ctx, &http.Client{Timeout: 30 * time.Second}, "https://api.github.com/repos/colony-2/c2j", version)
}
func resolve(ctx context.Context, client *http.Client, base, version string) (string, error) {
	if version == "" {
		version = "latest"
	}
	if !selector.MatchString(version) || len(version) > 200 {
		return "", fmt.Errorf("invalid c2j version")
	}
	if commit.MatchString(version) {
		return version, nil
	}
	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "pulse-tart-image-preparation")
		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("resolve c2j: %w", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("resolve c2j: GitHub returned HTTP %d", res.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
	}
	if version == "latest" {
		var release struct {
			Tag string `json:"tag_name"`
		}
		if err := get("/releases/latest", &release); err != nil {
			return "", err
		}
		version = release.Tag
		if !selector.MatchString(version) {
			return "", fmt.Errorf("invalid c2j release response")
		}
	}
	// Escape each segment so branch refs containing '/' remain usable.
	segments := strings.Split(version, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	var result struct {
		SHA string `json:"sha"`
	}
	if err := get("/commits/"+strings.Join(segments, "/"), &result); err != nil {
		return "", err
	}
	if !commit.MatchString(result.SHA) {
		return "", fmt.Errorf("invalid c2j commit response")
	}
	return result.SHA, nil
}
