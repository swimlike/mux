package mux

import (
	"context"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
)

// Muxer represents an HTTP request multiplexer.
// A Muxer is safe for concurrent use by multiple goroutines.
type Muxer struct {
	mu         sync.RWMutex
	registered map[string]*route
	routes     []*route
}

// NewMuxer creates and returns a new Muxer.
// The returned Muxer is safe for concurrent use by multiple goroutines.
func NewMuxer() *Muxer {
	return &Muxer{
		registered: make(map[string]*route, 10),
		routes:     make([]*route, 0, 10),
	}
}

// route represents a pattern with handlers.
type route struct {
	// the exploded pattern
	segments []string

	// the length of segments slice
	len int

	// paramateres names: segment index -> name
	params map[int]string

	// the handler for a pattern that ends in a slash
	slashHandler http.Handler

	// the handler for a pattern that NOT ends in a slash
	nonSlashHandler http.Handler
}

// notMatch checks whether the segment at index i
// does not match the pathSeg path segment.
func (p *route) notMatch(pathSeg string, i int) bool {
	if p.len == 0 || p.len-1 < i {
		return false
	}

	s := p.segments[i]
	return (s[0] != ':') && (s != pathSeg)
}

// params is a map for request parameter values.
type params map[string]string

// paramsMap returns a map containing request parameter values.
func (p *route) paramsMap(pathSegs []string) params {
	m := params{}
	slen := len(pathSegs)
	for i, name := range p.params {
		if i < slen {
			m[name] = pathSegs[i]
		}
	}
	return m
}

// priority computes the priority of the route.
//
// Every segment has a priority value:
// 2 = static segment
// 1 = dynamic segment
//
// The route priority is created by concatenating the priorities of the segments.
// The default (catch all) route has the priority 0.
func (p *route) priority() string {
	if p.len == 0 {
		return "0"
	}
	pri := ""
	for _, s := range p.segments {
		if s[0] == ':' {
			pri += "1"
		} else {
			pri += "2"
		}
	}
	return pri
}

// byPriority implements sort.Interface for []*route based on
// the priority().
type byPriority []*route

func (a byPriority) Len() int           { return len(a) }
func (a byPriority) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byPriority) Less(i, j int) bool { return a[i].priority() > a[j].priority() }

// Handle registers the handler for the given pattern.
//
// Static and dynamic patterns are supported.
// Static pattern examples:
//   /new
//   /
//   /products/
//
// Dynamic patterns can contain paramterer names after the colon character.
// Dynamic pattern examples:
//   /blog/:year/:month
//   /users/:username/profile
//
// Parameter values for a dynamic pattern will be available
// in the request's context (http.Request.Context()) associated with
// the parameter name. Use the context's Value() method to retrieve the values.
func (m *Muxer) Handle(pattern string, handler http.Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if pattern == "" {
		panic("invalid pattern " + pattern)
	}

	//host := ""
	//if pattern[0] != '/' {
	//	// the domain part of the url is case insensitive
	//	host = strings.ToLower(strings.Split(pattern, "/")[0])
	//}

	endsInSlash := pattern[len(pattern)-1] == '/'
	pattern = strings.Trim(pattern, "/")

	r := m.registered[pattern]
	if r == nil {
		r = &route{}
		if pattern != "" {
			r.segments = strings.Split(pattern, "/")
			r.len = len(r.segments)

			for i, s := range r.segments {
				if s[0] == ':' { // dynamic segment
					if r.params == nil {
						r.params = make(map[int]string)
					}
					r.params[i] = s[1:]
				}
			}
		}
		m.registered[pattern] = r
	}

	if endsInSlash {
		r.slashHandler = handler
	} else {
		r.nonSlashHandler = handler
	}

	m.routes = append(m.routes, r)
	sort.Sort(byPriority(m.routes))
}

// HandleFunc registers the handler function for the given pattern.
// See the Handle method for details on how to register a pattern.
func (m *Muxer) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if handler == nil {
		panic("nil handler")
	}
	m.Handle(pattern, http.HandlerFunc(handler))
}

// ServeHTTP dispatches the request to the handler whose
// pattern most closely matches the request URL.
//
// If the path is not in its canonical form, the
// handler will be an internally-generated handler
// that redirects to the canonical path.
func (m *Muxer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI == "*" {
		if r.ProtoAtLeast(1, 1) {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if r.Method != "CONNECT" {
		if p := cleanPath(r.URL.Path); p != r.URL.Path {
			url := *r.URL
			url.Path = p
			http.Redirect(w, r, url.String(), http.StatusMovedPermanently)
			return
		}
	}

	h, params := m.handler(r.Host, r.URL.Path)

	if len(params) > 0 {
		ctx := r.Context()
		for key, value := range params {
			ctx = context.WithValue(ctx, key, value)
		}
		r = r.WithContext(ctx)
	}

	h.ServeHTTP(w, r)
}

// Return the canonical path for p, eliminating . and .. elements.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	// path.Clean removes trailing slash except for root;
	// put the trailing slash back if necessary.
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// handler is the main implementation of Handler.
// The path is known to be in canonical form, except for CONNECT methods.
func (m *Muxer) handler(host, path string) (h http.Handler, params params) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if h == nil {
		h, params = m.match(host, path)
	}
	if h == nil {
		h, params = http.NotFoundHandler(), nil
	}
	return
}

func (m *Muxer) match(_ string, path string) (http.Handler, params) {
	endsInSlash := path[len(path)-1] == '/'
	segments := strings.Split(strings.Trim(path, "/"), "/")
	slen := len(segments)

	routes := m.possibleRoutes(slen, endsInSlash)

	var candidates []*route
LOOP:
	for i := slen - 1; i >= 0; i-- {
		s := segments[i]

		candidates = make([]*route, 0, len(routes))
		for _, r := range routes {
			if !r.notMatch(s, i) {
				candidates = append(candidates, r)
			}
		}
		if len(candidates) == 0 {
			break LOOP
		}
		routes = candidates
	}

	if len(candidates) > 0 {
		c := candidates[0]
		params := c.paramsMap(segments)
		if c.len < slen || endsInSlash {
			return c.slashHandler, params
		}
		return c.nonSlashHandler, params
	}

	return nil, nil
}

func (m *Muxer) possibleRoutes(slen int, endsInSlash bool) []*route {
	routes := make([]*route, 0, len(m.routes))
	for _, r := range m.routes {
		if r.len == slen && ((endsInSlash && r.slashHandler != nil) || (!endsInSlash && r.nonSlashHandler != nil)) {
			routes = append(routes, r)
		} else if r.len < slen && r.slashHandler != nil {
			routes = append(routes, r)
		}
	}
	return routes
}