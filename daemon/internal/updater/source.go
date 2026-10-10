// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

// Where releases are, and the bounds on what is read from there. The host is somebody else's: nothing it
// sends is trusted for its size or its shape, and nothing it says is believed until a signature says so.
const (
	// defaultListURL is this repository's release listing, newest first: what scripts/install.sh reads.
	// The list and not /releases/latest, which never names a pre-release.
	defaultListURL = "https://api.github.com/repos/Alexnex31/Norite/releases?per_page=30"
	// defaultDownloadBase is where a release's files are, under its tag.
	defaultDownloadBase = "https://github.com/Alexnex31/Norite/releases/download"

	maxListing   = 2 << 20
	maxReleases  = 100
	maxChecksums = 64 << 10
	maxBundle    = 256 << 10
	maxArchive   = 256 << 20
	maxRedirects = 5

	requestTimeout  = 30 * time.Second
	downloadTimeout = 15 * time.Minute
)

// ErrTooLarge is a response longer than its bound.
var ErrTooLarge = errors.New("the response is larger than a release's could be")

// Release is one published version.
type Release struct {
	// Tag is the release's tag, exactly: "v" and the version. It names the download and the identity the
	// signature is checked against.
	Tag     string
	Version gatewayproto.Version
}

// Source is where releases are listed and downloaded from.
type Source struct {
	ListURL      string
	DownloadBase string
	// UserAgent is the only thing a request says about this machine: the program and its version.
	UserAgent string
	Client    *http.Client
}

// DefaultSource is this repository's releases on GitHub, asked as the given daemon version.
func DefaultSource(version string) Source {
	return Source{
		ListURL:      defaultListURL,
		DownloadBase: defaultDownloadBase,
		UserAgent:    "norite-daemon/" + version,
		Client:       newHTTPClient(),
	}
}

// newHTTPClient follows a release download's redirect to wherever the files are kept, a bounded number of
// times, and never from HTTPS down to anything else. It sends no cookie and no credential: there is none.
func newHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("too many redirects")
			}
			if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return errors.New("a redirect left HTTPS")
			}
			return nil
		},
	}
}

// Releases reads the listing. What comes back is a hint about what exists and nothing more: the tags
// that are versions, in no order that is relied on. A draft is left out; whether a release is a
// pre-release is read from its version and never from the listing's word for it.
func (s Source) Releases(ctx context.Context) ([]Release, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	body, err := s.get(ctx, s.ListURL, maxListing)
	if err != nil {
		return nil, fmt.Errorf("reading the release listing: %w", err)
	}
	var listed []struct {
		Tag   string `json:"tag_name"`
		Draft bool   `json:"draft"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return nil, errors.New("the release listing is not a list of releases")
	}
	var out []Release
	for _, item := range listed {
		if len(out) == maxReleases {
			break
		}
		if release, ok := parseTag(item.Tag); ok && !item.Draft {
			out = append(out, release)
		}
	}
	return out, nil
}

// parseTag reads a release tag. Only the one spelling a release is published under counts: "v", then the
// version as SemVer writes it, with no build metadata, so the tag, the archive's name and the identity in
// the certificate are all the same text.
func parseTag(tag string) (Release, bool) {
	version, err := gatewayproto.ParseVersion(tag)
	if err != nil || tag != "v"+version.String() {
		return Release{}, false
	}
	return Release{Tag: tag, Version: version}, true
}

// get reads one small file whole, refusing one longer than limit.
func (s Source) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	resp, err := s.open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrTooLarge
	}
	return body, nil
}

// download copies one file to w, refusing one longer than limit, and returns how much it wrote.
func (s Source) download(ctx context.Context, url string, limit int64, w io.Writer) (int64, error) {
	resp, err := s.open(ctx, url)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, ErrTooLarge
	}
	return n, nil
}

func (s Source) open(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.UserAgent)
	req.Header.Set("Accept", "application/vnd.github+json, application/octet-stream")
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("the release host answered %d", resp.StatusCode)
	}
	return resp, nil
}

// fileURL is where one of a release's files is.
func (s Source) fileURL(r Release, name string) string {
	return s.DownloadBase + "/" + r.Tag + "/" + name
}
