package normurl

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

type Locator struct {
	url  *url.URL
	file bool
}

type jsonLocator struct {
	Url  string
	File bool
}

func (l *Locator) UnmarshalJSON(data []byte) error {
	var jl jsonLocator
	if err := json.Unmarshal(data, &jl); err != nil {
		return err
	}

	if jl.Url == "" {
		return fmt.Errorf("missing url")
	}

	nl, newErr := New(jl.Url)
	if newErr != nil {
		return newErr
	}

	if jl.File != nl.file {
		return fmt.Errorf("file flag mismatch")
	}

	l.file = jl.File
	l.url = nl.url

	return nil
}

func (l *Locator) MarshalJSON() ([]byte, error) {
	jl := jsonLocator{
		Url:  l.url.String(),
		File: l.file,
	}
	return json.Marshal(jl)
}

func (l *Locator) String() string {
	if l.file {
		return l.url.Path
	}
	return l.url.String()
}

func (l *Locator) SetQueryParam(param string, value string) {
	if l.file {
		return
	}
	query := l.url.Query()
	if value != "" {
		query.Set(param, value)
	} else {
		query.Del(param)
	}
	l.url.RawQuery = query.Encode()
}

func (l *Locator) IsFilepath() bool {
	return l.file
}

// Normalized returns a canonical string for the location.
//
// Two locations that identify the same resource produce the same result.
// Fragments are dropped, URL query parameters are sorted, scheme and host
// are lowercased, and "."/".." path segments are resolved.  For file paths,
// query parameters and fragments are dropped.
func (l *Locator) Normalized() string {
	if l.file {
		return filepath.Clean(l.url.Path)
	}

	u := *l.url
	u.Fragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)

	if u.RawQuery != "" {
		u.RawQuery = u.Query().Encode()
	}

	if u.Path == "" {
		u.Path = "/"
	}
	trailingSlash := strings.HasSuffix(u.Path, "/")
	u.Path = path.Clean(u.Path)
	if trailingSlash && u.Path != "/" {
		u.Path += "/"
	}
	return u.String()
}

func New(s string) (*Locator, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}

	if u.Scheme == "" {
		if !filepath.IsAbs(s) {
			return nil, fmt.Errorf("expected absolute path")
		}
		loc := &Locator{
			url:  u,
			file: true,
		}
		return loc, nil
	}

	if u.Scheme == "file" {
		path := u.Path
		if runtime.GOOS == "windows" {
			path = filepath.FromSlash(strings.TrimPrefix(path, "/"))
		}
		u.Scheme = ""
		u.Path = path
		loc := &Locator{
			url:  u,
			file: true,
		}
		return loc, nil
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %s", u.Scheme)
	}

	return &Locator{url: u}, nil
}

func (base *Locator) Resolve(s string) (*Locator, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}

	if u.Scheme != "" {
		return New(s)
	}

	if base.file {
		if filepath.IsAbs(s) {
			loc := &Locator{
				url:  u,
				file: true,
			}
			return loc, nil
		}

		baseDir := filepath.Dir(base.url.Path)
		path := filepath.Join(baseDir, s)
		loc := &Locator{
			url:  &url.URL{Path: path},
			file: true,
		}
		return loc, nil
	}

	resolved := base.url.ResolveReference(u)
	loc := &Locator{
		url:  resolved,
		file: false,
	}
	return loc, nil
}
