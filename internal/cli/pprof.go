package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	runtimepprof "runtime/pprof"
	runtimetrace "runtime/trace"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yusing/git-agent/internal/config"
)

func (a *App) maybeStartPprof(ctx context.Context, opts config.Options) error {
	if opts.Pprof == "" {
		return nil
	}
	listener, err := net.Listen("tcp", opts.Pprof)
	if err != nil {
		return fmt.Errorf("start pprof on %s: %w", opts.Pprof, err)
	}
	server := &http.Server{Handler: newPprofMux(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_, _ = fmt.Fprintf(a.stderr, "pprof error: %v\n", err)
		}
	}()
	return nil
}

func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprofIndex)
	mux.HandleFunc("/debug/pprof/cmdline", pprofCmdline)
	mux.HandleFunc("/debug/pprof/profile", pprofProfile)
	mux.HandleFunc("/debug/pprof/symbol", pprofSymbol)
	mux.HandleFunc("/debug/pprof/trace", pprofTrace)
	return mux
}

func pprofIndex(w http.ResponseWriter, r *http.Request) {
	const prefix = "/debug/pprof/"
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if name != "" {
		pprofNamedProfile(w, r, name)
		return
	}

	profiles := runtimepprof.Profiles()
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name() < profiles[j].Name() })
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, profile := range profiles {
		_, _ = fmt.Fprintf(w, "%s\n", profile.Name())
	}
}

func pprofNamedProfile(w http.ResponseWriter, r *http.Request, name string) {
	profile := runtimepprof.Lookup(name)
	if profile == nil {
		http.Error(w, "unknown profile", http.StatusNotFound)
		return
	}
	gc, _ := strconv.Atoi(r.URL.Query().Get("gc"))
	if name == "heap" && gc > 0 {
		runtime.GC()
	}
	debug, _ := strconv.Atoi(r.URL.Query().Get("debug"))
	if debug != 0 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	}
	if err := profile.WriteTo(w, debug); err != nil {
		_, _ = fmt.Fprintf(w, "write profile: %v\n", err)
	}
}

func pprofCmdline(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprint(w, strings.Join(os.Args, "\x00"))
}

func pprofProfile(w http.ResponseWriter, r *http.Request) {
	seconds := pprofSeconds(r, 30)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="profile"`)
	if err := runtimepprof.StartCPUProfile(w); err != nil {
		http.Error(w, "could not enable CPU profiling: "+err.Error(), http.StatusInternalServerError)
		return
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.Context().Done():
	}
	runtimepprof.StopCPUProfile()
}

func pprofTrace(w http.ResponseWriter, r *http.Request) {
	seconds := pprofSeconds(r, 1)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="trace"`)
	if err := runtimetrace.Start(w); err != nil {
		http.Error(w, "could not enable tracing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.Context().Done():
	}
	runtimetrace.Stop()
}

func pprofSymbol(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var input string
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read symbol request: "+err.Error(), http.StatusBadRequest)
			return
		}
		input = string(body)
	} else {
		input = r.URL.RawQuery
	}
	_, _ = fmt.Fprintln(w, "num_symbols: 1")
	for pcText := range strings.SplitSeq(input, "+") {
		pc, _ := strconv.ParseUint(pcText, 0, 64)
		if pc == 0 {
			continue
		}
		fn := runtime.FuncForPC(uintptr(pc))
		if fn != nil {
			_, _ = fmt.Fprintf(w, "%#x %s\n", pc, fn.Name())
		}
	}
}

func pprofSeconds(r *http.Request, fallback int64) int64 {
	seconds, err := strconv.ParseInt(r.URL.Query().Get("seconds"), 10, 64)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return seconds
}
