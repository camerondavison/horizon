package web

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/horizon/pkg/registry"
	"github.com/hashicorp/horizon/pkg/wire"
)

type HostnameChecker interface {
	HandlingHostname(name string) bool
}

type LabelResolver interface {
	FindLabelLink(labels []string) (string, []string, error)
	MatchServices(accid string, labels []string) ([]registry.ResolvedService, error)
}

type Connector interface {
	ConnectToService(req *wire.Request, accid string, rs registry.ResolvedService) (wire.Context, error)
}

type Frontend struct {
	L             hclog.Logger
	LabelResolver LabelResolver
	Connector     Connector
	Checker       HostnameChecker
}

func (f *Frontend) Serve(l net.Listener) error {
	return http.Serve(l, f)
}

func (f *Frontend) extractPrefixHost(host string) (string, string, bool) {
	var first, domain string

	firstDot := strings.IndexByte(host, '.')
	if firstDot != -1 {
		first = host[:firstDot]
		domain = host[firstDot:]
	} else {
		first = host
		domain = ""
	}

	lastDash := strings.LastIndexByte(first, '-')
	if lastDash == -1 {
		return "", "", false
	}

	return first[:lastDash+1] + domain, first[lastDash+1:], true
}

func (f *Frontend) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var (
		prefixHost, deployId string
		usingPrefix          bool
	)

	// Fail as fast as possible if we're not handling this host
	if !f.Checker.HandlingHostname(req.Host) {
		prefixHost, deployId, usingPrefix = f.extractPrefixHost(req.Host)
		if !usingPrefix {
			http.Error(w, fmt.Sprintf("no registered application for hostname: %s", req.Host), http.StatusInternalServerError)
			return
		}
	}

	f.L.Info("request",
		"target", req.Host,
		"method", req.Method,
		"path", req.URL.Path,
		"content-length", req.ContentLength,
	)

	labels := []string{":hostname=" + req.Host}

	account, target, err := f.LabelResolver.FindLabelLink(labels)
	if err != nil {
		f.L.Error("unable to resolve label link", "error", err, "hostname", req.Host)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(target) == 0 {
		// Look for a deployment oriented label target

		if usingPrefix {
			deployLabels := []string{":hostname=" + prefixHost, ":deployment=true"}
			account, target, err = f.LabelResolver.FindLabelLink(deployLabels)
			if err != nil || len(target) == 0 {
				http.Error(w, fmt.Sprintf("no registered application for hostname (2): %s", req.Host), http.StatusNotFound)
				return
			}

			target = append(target, ":deployment="+deployId)
		} else {
			http.Error(w, fmt.Sprintf("no registered application for hostname: %s", req.Host), http.StatusNotFound)
			return
		}
	}

	services, err := f.LabelResolver.MatchServices(account, target)
	if err != nil {
		f.L.Error("error resolving labels to services", "error", err, "labels", target)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rs := services[0]

	if rs.ServiceType != "http" {
		f.L.Error("service was not type http", "type", rs.ServiceType)
		http.Error(w, "no http services available", http.StatusNotFound)
		return
	}

	var wreq wire.Request
	wreq.Method = req.Method
	wreq.Path = req.URL.EscapedPath()
	wreq.Query = req.URL.RawQuery
	wreq.Fragment = req.URL.Fragment
	if user, pass, ok := req.BasicAuth(); ok {
		wreq.Auth = &wire.Auth{
			User:     user,
			Password: pass,
		}
	}

	for k, v := range req.Header {
		wreq.Headers = append(wreq.Headers, &wire.Header{
			Name:  k,
			Value: v,
		})
	}

	wctx, err := f.Connector.ConnectToService(&wreq, account, rs)
	if err != nil {
		f.L.Error("error connecting to service", "error", err, "labels", target)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	adapter := wctx.Writer()
	io.Copy(adapter, req.Body)
	adapter.Close()

	var wresp wire.Response

	tag, err := wctx.ReadMarshal(&wresp)
	if err != nil || tag != 1 {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	hdr := w.Header()

	for _, h := range wresp.Headers {
		for _, v := range h.Value {
			hdr.Add(h.Name, v)
		}
	}

	w.WriteHeader(int(wresp.Code))

	io.Copy(w, wctx.Reader())
}
