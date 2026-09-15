package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// version is stamped by build.sh.
var version = "dev"

func main() {
	var (
		addr      = flag.String("addr", ":8091", "HTTP listen address for the API and control UI")
		stateDir  = flag.String("state-dir", "/userdata/bd-play", "writable directory for the PDF page cache")
		scanEvery = flag.Duration("scan-interval", time.Second, "how often to look for USB storage changes")
		mediaDir  = flag.String("media-dir", "", "fixed media directory to offer alongside USB storage (a real stick takes priority)")
		autoplay  = flag.Bool("autoplay", false, "start playing the whole stick as soon as one is inserted")
		autoOrder = flag.String("autoplay-order", string(OrderSequential), "order for autoplay: order|random")
		autoDwell = flag.Duration("autoplay-dwell", DefaultImageDwell, "how long stills hold the screen during autoplay")
		patchUI   = flag.Bool("patch-ui", true, "add the USB entry to birdUI's source dropdown at startup")
		uiPath    = flag.String("ui-path", "/srv/birddog-web-ui/videoset.html", "birdUI template to patch")
		unpatchUI = flag.Bool("unpatch-ui", false, "remove the USB entry from birdUI and exit")
		showVer   = flag.Bool("version", false, "print version and exit")

		// The TV-remote mode: an entry in the decoder's own on-screen source
		// list (the one a CEC remote navigates) that opens a file browser,
		// which the same remote then drives.
		remoteOn    = flag.Bool("remote", true, "TV-remote mode: add an entry to the decoder's on-screen source list that opens a file browser driven over HDMI-CEC")
		cecDevice   = flag.String("cec-device", "/dev/cec0", "CEC adapter for the TV remote; empty leaves the browser to the API only")
		osdEntryNam = flag.String("osd-entry", DefaultOSDEntryName, "name of our entry in the decoder's SRT source list; empty adds none")
		osdPort     = flag.Int("osd-port", DefaultTriggerPort, "loopback UDP port the decoder dials when the entry is chosen")
		osdPipeline = flag.String("osd-pipeline", "auto", "how the browser reaches HDMI: auto, raw (fdsrc+rawvideoparse) or png (multifilesrc)")
		etcDir      = flag.String("etc", "/etc", "where the decoder keeps its source files")
		unregOSD    = flag.Bool("unregister-osd-entry", false, "remove our entry from the decoder's source list and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("bdplay", version)
		return
	}

	log := NewLogger()

	if *unpatchUI {
		if err := UnpatchVideoset(*uiPath); err != nil {
			fmt.Fprintln(os.Stderr, "unpatch:", err)
			os.Exit(1)
		}
		fmt.Println("removed the USB entry from", *uiPath)
		return
	}

	if *unregOSD {
		entry := osdEntry{etc: *etcDir, name: *osdEntryNam, port: *osdPort}
		changed, err := entry.Unregister()
		if err != nil {
			fmt.Fprintln(os.Stderr, "unregister:", err)
			os.Exit(1)
		}
		if changed {
			fmt.Println("removed", *osdEntryNam, "from", entry.listPath())
		} else {
			fmt.Println(*osdEntryNam, "was not in", entry.listPath())
		}
		return
	}

	log.Info("bdplay %s starting (addr=%s)", version, *addr)

	// Report what the device can and cannot do up front. These two lines are
	// the answer to most "why won't my stick play" questions.
	fs := probeFilesystems()
	log.Info("kernel filesystems: vfat=%v ntfs=%v exfat=%v", fs["vfat"], fs["ntfs"], fs["exfat"])

	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		log.Error("state dir %s: %v", *stateDir, err)
		os.Exit(1)
	}

	pdf := NewPDFRenderer(filepath.Join(*stateDir, "pdf-cache"), log)
	pdf.PruneCache(30 * 24 * time.Hour)

	display := NewDisplay(log)
	player := NewPlayer(display, pdf, log)
	storage := NewStorageManager(log)
	if *mediaDir != "" {
		storage.SetExtraDir(*mediaDir)
		log.Info("fixed media directory: %s", *mediaDir)
	}
	srv := NewServer(storage, player, pdf, log)

	if *patchUI {
		switch err := PatchVideoset(*uiPath, *addr); {
		case err == nil:
			log.Info("birdUI source dropdown now offers USB")
		case errors.Is(err, errAlreadyPatched):
			log.Info("birdUI already patched")
		default:
			// Not fatal: the daemon's own UI on :8091 still works, so a
			// firmware layout we do not recognise costs the integration, not
			// the feature.
			log.Warn("could not patch birdUI (%v); use the control page at http://%s:%s/ instead",
				err, hostname(), portOf(*addr))
		}
	}

	stop := make(chan struct{})

	// The TV-remote session and the two ways into it: the decoder dialling
	// our entry, and the API.
	var remote *Remote
	if *remoteOn {
		screen := NewScreen(*osdPipeline, filepath.Join(*stateDir, "osd"), log)
		remote = NewRemote(RemoteConfig{
			CECPath:        *cecDevice,
			OSDName:        "USB Player",
			AnnounceActive: true,
			Dwell:          *autoDwell,
			Etc:            *etcDir,
		}, display, player, screen, srv.Library, storage.Primary, DetectMode, log)
		srv.remote = remote

		if *osdEntryNam != "" {
			entry := osdEntry{etc: *etcDir, name: *osdEntryNam, port: *osdPort}
			switch changed, err := entry.Register(); {
			case err != nil:
				log.Warn("could not add %q to the decoder's source list (%v); use the API or web page to open the browser", *osdEntryNam, err)
			case changed:
				log.Info("added %q to the decoder's on-screen source list (SRT page)", *osdEntryNam)
			default:
				log.Info("%q is in the decoder's on-screen source list", *osdEntryNam)
			}
			guard := newSourceGuard(*etcDir, entry, log)
			remote.guard = guard
			go guard.Watch(stop, 2*time.Second)

			trigger := newTriggerListener(*osdPort, log, func() {
				go func() {
					if err := remote.Enter("selected on the decoder's menu"); err != nil {
						log.Error("remote: %v", err)
					}
				}()
			})
			remote.trigger = trigger
			go func() {
				if err := trigger.Run(stop); err != nil {
					log.Error("trigger listener on 127.0.0.1:%d: %v — the on-screen entry will not open the browser", *osdPort, err)
				}
			}()
		}
		if *cecDevice != "" && !cecDeviceExists(*cecDevice) {
			log.Warn("%s not found: the browser will open but the TV remote cannot drive it", *cecDevice)
		}
	}

	// Scan once up front. Watch only fires its callback when the volume set
	// changes, so without this a fixed -media-dir (which never "changes")
	// would present an empty library until a stick was inserted.
	srv.Rescan()

	// Watch USB and keep the library in step.
	go storage.Watch(stop, *scanEvery, func() {
		srv.Rescan()
		if remote != nil && remote.Active() {
			// The browser redraws; autoplay would fight it for the display.
			remote.Refresh()
			return
		}
		if !*autoplay {
			return
		}
		lib, _ := srv.Library()
		if lib == nil {
			// Stick removed: stop rather than leaving the last frame up.
			player.Stop()
			return
		}
		items := lib.Select("", true)
		if len(items) == 0 {
			return
		}
		log.Info("autoplay: %d item(s)", len(items))
		if err := player.Start(items, Options{
			Order:      Order(*autoOrder),
			Loop:       true,
			ImageDwell: *autoDwell,
		}); err != nil {
			log.Error("autoplay: %v", err)
		}
	})

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http: %v", err)
			os.Exit(1)
		}
	}()
	log.Info("control UI on http://%s:%s/", hostname(), portOf(*addr))

	// Shut down cleanly. The important part is player.Stop(), which releases
	// the display and restarts BirdDogRunner — without it, a systemctl restart
	// of bdplay would leave the unit dark and looking bricked.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Info("signal %v: shutting down", s)

	close(stop)
	if remote != nil {
		remote.Exit()
	}
	player.Stop()
	display.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	log.Info("stopped")
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}
