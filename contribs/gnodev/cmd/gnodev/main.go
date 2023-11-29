package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	gnodev "github.com/gnolang/gno/contribs/gnodev/pkg/dev"
	"github.com/gnolang/gno/gno.land/pkg/gnoweb"
	"github.com/gnolang/gno/gnovm/pkg/gnoenv"
	"github.com/gnolang/gno/gnovm/pkg/gnomod"
	"github.com/gnolang/gno/tm2/pkg/commands"
	tmlog "github.com/gnolang/gno/tm2/pkg/log"
	osm "github.com/gnolang/gno/tm2/pkg/os"
)

type devCfg struct {
	bindAddr string
	minimal  bool
	verbose  bool
	noWatch  bool
}

var defaultDevOptions = &devCfg{
	bindAddr: "127.0.0.1:8888",
}

func main() {
	cfg := &devCfg{}

	stdio := commands.NewDefaultIO()
	cmd := commands.NewCommand(
		commands.Metadata{
			Name:       "gnodev",
			ShortUsage: "gnodev [flags] <path>",
			ShortHelp:  "GnoDev run a node for dev purpose, it will load the given package path",
		},
		cfg,
		func(_ context.Context, args []string) error {
			return execDev(cfg, args, stdio)
		})

	if err := cmd.ParseAndRun(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
}

func (c *devCfg) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(
		&c.bindAddr,
		"web-bind",
		defaultDevOptions.bindAddr,
		"verbose output when deving",
	)

	fs.BoolVar(
		&c.minimal,
		"minimal",
		defaultDevOptions.verbose,
		"don't load example folder packages along default transaction",
	)

	fs.BoolVar(
		&c.verbose,
		"verbose",
		defaultDevOptions.verbose,
		"verbose output when deving",
	)

	fs.BoolVar(
		&c.noWatch,
		"no-watch",
		defaultDevOptions.noWatch,
		"watch for files change",
	)

}

func execDev(cfg *devCfg, args []string, io commands.IO) error {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// guess root dir
	gnoroot := gnoenv.RootDir()

	pkgpaths, err := parseArgsPackages(io, args)
	if err != nil {
		return fmt.Errorf("unable to parse package paths: %w", err)
	}

	// logger := log.NewTMLogger(log.NewSyncWriter(io.Out))
	logger := tmlog.NewNopLogger()

	// RAWTerm setup
	rt := gnodev.NewRawTerm()
	{
		restore, err := rt.Init()
		if err != nil {
			return fmt.Errorf("unable to init raw term for dev: %s", err)
		}
		defer restore()

		// correctly format output for terminal
		io.SetOut(commands.WriteNopCloser(rt))

		// Setup trap signal
		osm.TrapSignal(func() {
			restore()
			cancel(nil)
		})
	}

	var dnode *gnodev.Node
	{
		var err error
		dnode, err = setupDevNode(ctx, logger, cfg, pkgpaths, gnoroot)
		if err != nil {
			return err // already formated in setupDevNode
		}
		defer dnode.Close()
	}

	rt.Taskf("Node", "Listener: %s", dnode.GetRemoteAddress())

	// setup files watcher
	w, err := setupPkgsWatcher(cfg, dnode.ListPkgs())
	if err != nil {
		return fmt.Errorf("unable to watch for files change: %w", err)
	}

	ccpath := make(chan []string, 1)
	go func() {
		defer close(ccpath)

		const debounceTimeout = time.Millisecond * 500

		if err := handleDebounce(ctx, w, ccpath, debounceTimeout); err != nil {
			cancel(err)
		}
	}()

	// Gnoweb setup
	server := setupGnowebServer(cfg, dnode, rt)

	l, err := net.Listen("tcp", cfg.bindAddr)
	if err != nil {
		return fmt.Errorf("unable to listen to %q: %w", cfg.bindAddr, err)
	}

	go func() {
		var err error
		if srvErr := server.Serve(l); srvErr != nil {
			err = fmt.Errorf("HTTP server stopped with error: %w", srvErr)
		}
		cancel(err)
	}()
	defer server.Close()

	rt.Taskf("GnoWeb", "Listener: %s", l.Addr())

	// Print basic infos
	rt.Taskf("Node", "Default Address: %s", gnodev.DefaultCreator.String())
	rt.Taskf("Node", "Chain ID: %s", dnode.Config().ChainID())

	rt.Taskf("[Ready]", "for commands and help, press `h`")

	cckey := listenForKeyPress(io, rt)
	for {
		var err error

		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case paths := <-ccpath:
			for _, path := range paths {
				rt.Taskf("HotReload", "path %q has been modified\n", path)
			}

			rt.Taskf("Node", "Loading package updates...")
			if err = dnode.UpdatePackages(paths...); err != nil {
				checkForError(rt, err)
				continue
			}

			rt.Taskf("Node", "Reloading...")
			err = dnode.Reload(ctx)
			checkForError(rt, err)
		case key, ok := <-cckey:
			if !ok {
				cancel(nil)
				continue
			}

			if cfg.verbose {
				rt.Taskf("KeyPress", "<%s>", key.String())
			}

			switch key {
			case 'h', 'H':
				printHelper(io)
			case 'r', 'R':
				rt.Taskf("Node", "Reloading all packages...")
				err = dnode.ReloadAll(ctx)
				checkForError(rt, err)
			case gnodev.KeyCtrlR:
				rt.Taskf("Node", "Reseting state...")
				err = dnode.Reset(ctx)
				checkForError(rt, err)
			case gnodev.KeyCtrlC:
				cancel(nil)
			default:
			}
		}
	}
}

// XXX: Automatize this the same way command does
func printHelper(io commands.IO) {
	io.Println(`
Gno Dev Helper:
  h, H        Help - display this message
  r, R        Reload - Reload all packages to take change into account.
  Ctrl+R      Reset - Reset application state.
  Ctrl+C      Exit - Exit the application
`)
}

func handleDebounce(ctx context.Context, watcher *fsnotify.Watcher, changedPathsCh chan<- []string, timeout time.Duration) error {
	var debounceTimer <-chan time.Time
	pathList := []string{}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case watchErr := <-watcher.Errors:
			return fmt.Errorf("watch error: %w", watchErr)
		case <-debounceTimer:
			changedPathsCh <- pathList
			// Reset pathList and debounceTimer
			pathList = []string{}
			debounceTimer = nil
		case evt := <-watcher.Events:
			if evt.Op == fsnotify.Write {
				pathList = append(pathList, evt.Name)
				debounceTimer = time.After(timeout)
			}
		}
	}
}

func setupPkgsWatcher(cfg *devCfg, pkgs []gnomod.Pkg) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("unable to watch files: %w", err)
	}

	if cfg.noWatch {
		// noop watcher
		return watcher, nil
	}

	for _, pkg := range pkgs {
		if err := watcher.Add(pkg.Dir); err != nil {
			return nil, fmt.Errorf("unable to watch %q: %w", pkg.Dir, err)
		}
	}

	return watcher, nil
}

// setupDevNode initializes and returns a new DevNode.
func setupDevNode(ctx context.Context, logger tmlog.Logger, cfg *devCfg, pkgspath []string, gnoroot string) (*gnodev.Node, error) {
	if !cfg.minimal {
		examplesDir := filepath.Join(gnoroot, "examples")
		pkgspath = append(pkgspath, examplesDir)
	}

	return gnodev.NewDevNode(ctx, logger, pkgspath)
}

// setupGnowebServer initializes and starts the Gnoweb server.
func setupGnowebServer(cfg *devCfg, dnode *gnodev.Node, rt *gnodev.RawTerm) *http.Server {
	var server http.Server

	webConfig := gnoweb.NewDefaultConfig()
	webConfig.RemoteAddr = dnode.GetRemoteAddress()

	loggerweb := log.New(rt, "gnoweb: ", log.LstdFlags)
	app := gnoweb.MakeApp(loggerweb, webConfig)

	server.ReadHeaderTimeout = 60 * time.Second
	server.Handler = app.Router

	return &server
}

func parseArgsPackages(io commands.IO, args []string) (paths []string, err error) {
	paths = make([]string, len(args))
	for i, arg := range args {
		abspath, err := filepath.Abs(arg)
		if err != nil {
			return nil, fmt.Errorf("invalid path %q: %w", arg, err)
		}

		ppath, err := gnomod.FindRootDir(abspath)
		if err != nil {
			return nil, fmt.Errorf("unable to find root dir of %q: %w", abspath, err)
		}

		paths[i] = ppath
	}

	return paths, nil
}

func listenForKeyPress(io commands.IO, rt *gnodev.RawTerm) <-chan gnodev.KeyPress {
	cc := make(chan gnodev.KeyPress, 1)
	go func() {
		defer close(cc)
		for {
			key, err := rt.ReadKeyPress()
			if err != nil {
				io.ErrPrintfln("unable to read keypress: %s", err.Error())
				return
			}

			cc <- key
		}
	}()

	return cc
}

func checkForError(rt *gnodev.RawTerm, err error) {
	if err != nil {
		rt.Taskf("", "[ERROR] - %s", err.Error())
	} else {
		rt.Taskf("", "[DONE]")
	}
}