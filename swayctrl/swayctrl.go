package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cespare/subcmd"
	"github.com/joshuarubin/go-sway"
	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
)

var cmds = []subcmd.Command{
	{
		Name:        "launch",
		Description: "launch an app and focus it",
		Do:          cmdLaunch,
	},
	{
		Name:        "focus",
		Description: "focus a particular window (or optionally launch the app)",
		Do:          cmdFocus,
	},
	{
		Name:        "tree",
		Description: "print a subset of the sway tree",
		Do:          cmdTree,
	},
	{
		Name:        "appnext",
		Description: "focus the next instance of the same app",
		Do:          cmdAppNext,
	},
	{
		Name:        "prev",
		Description: "focus the previously focused window",
		Do:          cmdPrev,
	},
	{
		Name:        "focustitle",
		Description: "print the titles of the currently focused node whenever focus changes",
		Do:          cmdFocusTitle,
	},
	{
		Name:        "swaymsg",
		Description: "run swaymsg with the correct SWAYSOCK",
		Do:          cmdSwaymsg,
	},
	{
		Name:        "daemon",
		Description: "run subscriber daemon",
		Do:          cmdDaemon,
	},
}

func main() {
	log.SetFlags(0)

	// Make swayctrl work even if SWAYSOCK isn't set correctly (e.g., from
	// inside a tmux session that has been running for a while).
	// We don't use sway.WithSocketPath because sway.Subscribe doesn't have
	// a corresponding way to configure it :\
	if _, err := os.Stat(os.Getenv("SWAYSOCK")); err != nil {
		u, err := user.Current()
		if err != nil {
			log.Fatal(err)
		}
		glob := fmt.Sprintf("/run/user/%s/sway-ipc.%[1]s.*.sock", u.Uid)
		files, err := filepath.Glob(glob)
		if err != nil {
			log.Fatalf("Error discovering sway socket file: %s", err)
		}
		if len(files) == 0 {
			log.Fatal("Cannot discover sway socket file")
		}
		if len(files) > 1 {
			log.Fatalf("Multiple socket files matching pattern %s", glob)
		}
		os.Setenv("SWAYSOCK", files[0])
	}

	subcmd.Run(cmds)
}

func newClient(ctx context.Context) sway.Client {
	client, err := sway.New(ctx)
	if err != nil {
		log.Fatal(err)
	}
	return client
}

func cmdLaunch(args []string) {
	fs := flag.NewFlagSet("launch", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:

  swayctrl launch <cmd> [args...]

The launch command launches an application then focuses the newly-launched
application's window.
`)
	}
	fs.Parse(args)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}

	ctx := context.Background()
	client := newClient(ctx)
	launchAndFocus(ctx, client, fs.Args()[0], fs.Args()[1:]...)
}

func cmdFocus(args []string) {
	fs := flag.NewFlagSet("focus", flag.ExitOnError)
	title := fs.String("title", "", "Window title (regex match)")
	appID := fs.String("appid", "", "App ID (exact match)")
	launchCmd := fs.String("launch", "", "Launch if window doesn't exist (optional)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:

  swayctrl focus [flags...]

where the flags are:
`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
The focus command selects and focuses the first matching window (in tree order).
It prioritizes visible windows over not-visible ones and, as a secondary
preference, more recently focused windows. The daemon must be running.

If -launch is given, use that command (passed to /bin/sh -c) to launch the
application if focusing it fails.
`)
	}
	fs.Parse(args)

	if *title == "" && *appID == "" {
		log.Fatalln("At least one of -title or -appid is required")
	}
	var titleRE *regexp.Regexp
	if *title != "" {
		var err error
		titleRE, err = regexp.Compile(*title)
		if err != nil {
			log.Fatalln("Bad -title regex:", err)
		}
	}

	mruList := getMRUListFromDaemon()
	idToMRUIdx := make(map[int64]int)
	for i, w := range mruList {
		idToMRUIdx[w.ID] = i
	}

	ctx := context.Background()
	client := newClient(ctx)
	pick := func(n *sway.Node) bool {
		switch n.Type {
		case sway.NodeCon, sway.NodeFloatingCon:
		default:
			return false
		}
		if titleRE != nil && !titleRE.MatchString(n.Name) {
			return false
		}
		if *appID != "" && (n.AppID == nil || *n.AppID != *appID) {
			return false
		}
		return true
	}

	if focusExisting(ctx, client, idToMRUIdx, pick) {
		return
	}
	if *launchCmd == "" {
		log.Fatalln("No match")
	}
	log.Printf("Running %q", *launchCmd)
	launchAndFocus(ctx, client, "/bin/sh", "-c", *launchCmd)
}

func focusExisting(ctx context.Context, client sway.Client, idToMRUIdx map[int64]int, pick func(n *sway.Node) bool) (ok bool) {
	root, err := client.GetTree(ctx)
	if err != nil {
		log.Fatalln("GET_TREE failed:", err)
	}
	matches := treeSelect(root, pick)
	if len(matches) == 0 {
		return false
	}
	// Prioritize visible windows.
	slices.SortStableFunc(matches, func(n0, n1 *sway.Node) bool {
		if *n0.Visible != *n1.Visible {
			return *n0.Visible
		}
		i0, ok0 := idToMRUIdx[n0.ID]
		i1, ok1 := idToMRUIdx[n1.ID]
		if ok0 != ok1 {
			return ok0
		}
		if !ok0 {
			return false
		}
		return i0 < i1
	})
	log.Printf("Focusing con_id %d", matches[0].ID)
	command := fmt.Sprintf("[con_id=%d] focus", matches[0].ID)
	if err := runCommand(ctx, client, command); err != nil {
		log.Fatalf("Error running command %q: %s", command, err)
	}
	return true
}

// launchAndFocus launches an app using the given command and then focuses the
// window.
func launchAndFocus(ctx context.Context, client sway.Client, command string, args ...string) {
	root, err := client.GetTree(ctx)
	if err != nil {
		log.Fatalln("GET_TREE failed:", err)
	}
	oldIDs := make(map[int64]struct{})
	walkTree(root, func(n *sway.Node) {
		oldIDs[n.ID] = struct{}{}
	})
	getNewID := func() int64 {
		root, err := client.GetTree(ctx)
		if err != nil {
			log.Fatalln("GET_TREE failed:", err)
		}
		newID := int64(-1)
		walkTree(root, func(n *sway.Node) {
			if _, ok := oldIDs[n.ID]; !ok {
				if newID == -1 || n.ID < newID {
					newID = n.ID
				}
			}
		})
		return newID
	}
	launch(command, args...)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for range ticker.C {
		if time.Since(start) > 1200*time.Millisecond {
			log.Fatalln("Application couldn't be focused after launch")
		}
		newID := getNewID()
		if newID < 0 {
			continue
		}
		log.Printf("Focusing con_id %d", newID)
		command := fmt.Sprintf("[con_id=%d] focus", newID)
		if err := runCommand(ctx, client, command); err != nil {
			log.Fatalf("Error running command %q: %s", command, err)
		}
		return
	}
}

func launch(command string, args ...string) {
	cmd := exec.Command(command, args...)
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Fatalf("Error launching %q: %s", command, err)
	}
}

func cmdTree(args []string) {
	fs := flag.NewFlagSet("tree", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:

  swayctrl tree

The tree command prints a subset of the sway tree structure that has a lot of
the relevant information for scripting but is much less verbose.
`)
	}
	fs.Parse(args)

	ctx := context.Background()
	client := newClient(ctx)
	root, err := client.GetTree(ctx)
	if err != nil {
		log.Fatalln("GET_TREE failed:", err)
	}

	var depth int
	printNode := func(n *sway.Node) {
		var b strings.Builder
		b.WriteString(strings.Repeat("  ", depth))
		fmt.Fprintf(&b, "[%d:%s]", n.ID, n.Type)
		if n.AppID != nil && *n.AppID != "" {
			fmt.Fprintf(&b, " [app_id:%s]", *n.AppID)
		}
		if n.Shell != nil && *n.Shell != "xdg_shell" {
			fmt.Fprintf(&b, " [shell:%s]", *n.Shell)
		}
		if n.Name != "" {
			fmt.Fprintf(&b, " | %s", n.Name)
		}
		fmt.Println(b.String())
	}
	var walk func(*sway.Node)
	walk = func(node *sway.Node) {
		printNode(node)
		depth++
		for _, n := range node.FloatingNodes {
			walk(n)
		}
		for _, n := range node.Nodes {
			walk(n)
		}
		depth--
	}
	walk(root)
}

func cmdAppNext(args []string) {
	fs := flag.NewFlagSet("appnext", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:

  swayctrl appnext

The appnext command focuses the next instance of the focused application (if
another one exists).
`)
	}
	fs.Parse(args)

	ctx := context.Background()
	client := newClient(ctx)
	root, err := client.GetTree(ctx)
	if err != nil {
		log.Fatalln("GET_TREE failed:", err)
	}
	focused := root.FocusedNode()
	if focused == nil {
		log.Fatal("No focused node")
	}
	if focused.AppID == nil {
		log.Fatal("Focused node is not a container with an app ID")
	}
	matches := treeSelect(root, func(n *sway.Node) bool {
		return n.AppID != nil && *n.AppID == *focused.AppID
	})
	if len(matches) < 2 {
		return
	}
	i := slices.IndexFunc(matches, func(n *sway.Node) bool { return n == focused })
	if i < 0 {
		log.Fatal("Inconsistent tree?")
	}
	j := (i + 1) % len(matches)
	command := fmt.Sprintf("[con_id=%d] focus", matches[j].ID)
	if err := runCommand(ctx, client, command); err != nil {
		log.Fatalf("Error running command %q: %s", command, err)
	}
}

func cmdPrev(args []string) {
	fs := flag.NewFlagSet("prev", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:

  swayctrl prev

The prev command focuses the previously focused window. The daemon must be running.
`)
	}
	fs.Parse(args)

	mruList := getMRUListFromDaemon()

	ctx := context.Background()
	client := newClient(ctx)
	root, err := client.GetTree(ctx)
	if err != nil {
		log.Fatalln("GET_TREE failed:", err)
	}
	focused := root.FocusedNode()
	if focused == nil {
		log.Fatal("No focused node")
	}
	for _, w := range mruList {
		if w.ID == focused.ID {
			continue
		}
		command := fmt.Sprintf("[con_id=%d] focus", w.ID)
		if err := runCommand(ctx, client, command); err != nil {
			log.Fatalf("Error running command %q: %s", command, err)
		}
		return
	}
	log.Println("No other window to focus")
}

func getMRUListFromDaemon() []listWindow {
	sockDir := os.Getenv("XDG_RUNTIME_DIR")
	if sockDir == "" {
		log.Fatalln("XDG_RUNTIME_DIR must be defined (to place socket file)")
	}
	sockPath := filepath.Join(sockDir, "swayctrl.sock")
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				// Don't bother sending the addr through the URL.
				return net.Dial("unix", sockPath)
			},
		},
	}
	resp, err := hc.Get("http://localhost/") // fake
	if err != nil {
		log.Fatalln("Error querying local daemon (is it running?):", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Fatalf("Got a non-200 status code (%d) from daemon", resp.StatusCode)
	}
	var mruList []listWindow
	if err := json.NewDecoder(resp.Body).Decode(&mruList); err != nil {
		log.Fatalln("Error reading most recently used window list from daemon:", err)
	}
	return mruList
}

func cmdFocusTitle(args []string) {
	fs := flag.NewFlagSet("title", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:

  swayctrl title

The title command prints the title of the currently focused window.
`)
	}
	fs.Parse(args)

	ctx := context.Background()
	client := newClient(ctx)

	root, err := client.GetTree(ctx)
	if err != nil {
		log.Fatalln("GET_TREE failed:", err)
	}
	focused := root.FocusedNode()
	if focused == nil {
		log.Fatal("No focused node")
	}
	printTitle(focused)

	// TODO: there's a race here where we could miss a focus event.

	handler := newFocusHandler()
	if err := sway.Subscribe(ctx, handler, sway.EventTypeWindow); err != nil {
		log.Fatalln("Error with subscription:", err)
	}
}

func printTitle(n *sway.Node) {
	if n.Shell != nil && *n.Shell != "xdg_shell" {
		fmt.Printf("[%s] %s\n", *n.Shell, n.Name)
		return
	}
	fmt.Println(n.Name)
}

type focusHandler struct {
	sway.EventHandler
}

func newFocusHandler() *focusHandler {
	h := &focusHandler{
		EventHandler: sway.NoOpEventHandler(),
	}
	return h
}

func (h *focusHandler) Window(ctx context.Context, e sway.WindowEvent) {
	switch e.Change {
	case sway.WindowFocus, sway.WindowTitle:
		if e.Container.Focused {
			printTitle(&e.Container)
		}
	case sway.WindowClose:
		if e.Container.Focused {
			fmt.Println()
		}
	}
}

func cmdSwaymsg(args []string) {
	// Don't use flag here because we want to be able to write (for example)
	//
	// 	swayctrl swaymsg -v
	//
	// rather than
	//
	// 	swayctrl swaymsg -- -v
	//
	// Print out the help text if no args were provided, though ('swaymsg'
	// by itself does nothing and prints nothing).
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, `Usage:

swayctrl swaymsg [flags...]

The swaymsg command simply runs swaymsg, but it sets the correct SWAYSOCK
environment variable.
`)
		os.Exit(2)
	}

	cmd := exec.Command("swaymsg", args...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.Exited() {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		log.Fatal(err)
	}
}

func cmdDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	verbose := fs.Bool("v", false, "Verbose mode")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:

  swayctrl daemon [-v]

The daemon command starts a long-running process that subscribes to sway IPC
events and tracks window focus history. This is necessary for the 'prev' command.

The -v flag enables verbose mode where the daemon logs its actions.
`)
	}
	fs.Parse(args)

	ctx := context.Background()
	sockDir := os.Getenv("XDG_RUNTIME_DIR")
	if sockDir == "" {
		log.Fatalln("XDG_RUNTIME_DIR must be defined (to place socket file)")
	}
	lock := lockFile(filepath.Join(sockDir, "swayctrl.lock"))
	defer lock.unlock()
	handler := newDaemonHandler(*verbose)
	handler.listen(filepath.Join(sockDir, "swayctrl.sock"))
	if err := sway.Subscribe(ctx, handler, sway.EventTypeWindow); err != nil {
		log.Fatalln("Error with subscription:", err)
	}
}

type daemonHandler struct {
	verbose bool
	mu      sync.Mutex
	list    windowMRUList
	sway.EventHandler
}

func newDaemonHandler(verbose bool) *daemonHandler {
	h := &daemonHandler{
		EventHandler: sway.NoOpEventHandler(),
		verbose:      verbose,
	}
	h.list.m = make(map[int64]*mruElt)
	return h
}

func (h *daemonHandler) listen(sockPath string) {
	if err := os.RemoveAll(sockPath); err != nil {
		log.Fatalln("Error creating socket file:", err)
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatalln("Error listening with socket file:", err)
	}
	handle := func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		nodes := h.list.all()
		h.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodes)
	}
	go func() {
		log.Fatal("Serve error:", http.Serve(l, http.HandlerFunc(handle)))
	}()
}

func (h *daemonHandler) Window(ctx context.Context, e sway.WindowEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch e.Change {
	case sway.WindowFocus:
		appID := "?"
		if e.Container.AppID != nil && *e.Container.AppID != "" {
			appID = *e.Container.AppID
		}
		h.list.bringFront(e.Container.ID, appID)
	case sway.WindowClose:
		h.list.delete(e.Container.ID)
	default:
		return
	}
	if h.verbose {
		log.Printf("Event[%s]: %v", e.Change, h.list.all())
	}
}

type windowMRUList struct {
	head *mruElt
	m    map[int64]*mruElt
}

type mruElt struct {
	prev  *mruElt
	next  *mruElt
	id    int64
	appID string
}

func (l *windowMRUList) bringFront(id int64, appID string) {
	e, ok := l.m[id]
	if !ok {
		e = &mruElt{id: id}
		l.m[id] = e
	}
	e.appID = appID
	if l.head == e {
		return
	}

	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}

	e.prev = nil
	e.next = l.head
	if e.next != nil {
		e.next.prev = e
	}

	l.head = e
}

func (l *windowMRUList) delete(id int64) {
	e, ok := l.m[id]
	if !ok {
		return
	}
	delete(l.m, id)
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	if l.head == e {
		l.head = e.next
	}
}

func (l *windowMRUList) all() []listWindow {
	var nodes []listWindow
	var i int
	for e := l.head; e != nil; e = e.next {
		i++
		// FIXME: delete
		if i > 100 {
			l.debug()
			panic("boom")
		}
		nodes = append(nodes, listWindow{e.id, e.appID})
	}
	return nodes
}

type listWindow struct {
	ID    int64
	AppID string
}

func (s listWindow) String() string {
	return fmt.Sprintf("%d:%s", s.ID, s.AppID)
}

func (l *windowMRUList) debug() {
	for id, e := range l.m {
		fmt.Printf("%d -> %p\n", id, e)
	}
	var i int
	for e := l.head; e != nil; e = e.next {
		i++
		if i == 30 {
			fmt.Printf("quitting after 30")
			break
		}
		fmt.Printf("[%d] prev=%p next=%p\n", e.id, e.prev, e.next)
	}
}

func treeSelect(node *sway.Node, fn func(*sway.Node) bool) []*sway.Node {
	var matches []*sway.Node
	walkTree(node, func(n *sway.Node) {
		if fn(n) {
			matches = append(matches, n)
		}
	})
	return matches
}

func walkTree(node *sway.Node, fn func(*sway.Node)) {
	fn(node)
	for _, n := range node.FloatingNodes {
		walkTree(n, fn)
	}
	for _, n := range node.Nodes {
		walkTree(n, fn)
	}
}

func runCommand(ctx context.Context, client sway.Client, command string) error {
	results, err := client.RunCommand(ctx, command)
	if err != nil {
		return err
	}
	for i, res := range results {
		if !res.Success {
			return fmt.Errorf("command %d failed: %s", i, res.Error)
		}
	}
	return nil
}

type fileLock struct {
	f *os.File
}

func lockFile(path string) *fileLock {
	f, err := os.OpenFile(path, os.O_CREATE, 0o644)
	if err != nil {
		log.Fatalln("Error creating lock file:", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		log.Fatal("Lockfile locked (is another instance running?)")
	}
	return &fileLock{f: f}
}

func (l *fileLock) unlock() {
	// Ignore these errors -- we're about to exit.
	// (We still defer unlock to avoid early GC -> accidental unlocking.)
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	_ = l.f.Close()
}
