// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"gonetflow"
	"io/ioutil"
	"ipmac"
	"minicli"
	log "minilog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

const (
	DisconnectedVLAN = -1
	TrunkVLAN        = -2
)

// Bridge represents a bridge on the host and the Taps connected to it.
type Bridge struct {
	Name     string
	preExist bool
	iml      *ipmac.IPMacLearner
	nf       *gonetflow.Netflow
	Trunk    []string
	Tunnel   []string

	Taps map[string]Tap

	// Embedded mutex
	sync.Mutex

	// Guards startIML
	once sync.Once
}

type Tap struct {
	lan  int
	host bool
}

const (
	DEFAULT_BRIDGE = "mega_bridge"
	OVS_TIMEOUT    = time.Duration(5 * time.Second)
	TYPE_VXLAN     = 1
	TYPE_GRE       = 2
)

var (
	bridges map[string]*Bridge // all bridges. mega_bridge0 will be automatically added

	tapNameChan chan string // atomic feeder of tap names

	bridgeLock sync.Mutex
	ovsLock    sync.Mutex
)

// create the default bridge struct and create a goroutine to generate
// tap names for this host.
func init() {
	bridges = make(map[string]*Bridge)

	tapNameChan = make(chan string)

	go func() {
		for tapCount := 0; ; tapCount++ {
			tapName := fmt.Sprintf("mega_tap%v", tapCount)
			fpath := filepath.Join("/sys/class/net", tapName)

			if _, err := os.Stat(fpath); os.IsNotExist(err) {
				tapNameChan <- tapName
			} else if err != nil {
				log.Fatal("unable to stat file -- %v %v", fpath, err)
			}

			log.Debug("tapCount: %v", tapCount)
		}
	}()
}

// NewBridge creates a new bridge with ovs, assumes that the bridgeLock is held.
func NewBridge(name string) (*Bridge, error) {
	log.Debug("creating new bridge -- %v", name)
	b := &Bridge{
		Name: name,
		Taps: make(map[string]Tap),
	}

	// Create the bridge
	isNew, err := ovsAddBridge(b.Name)
	if err != nil {
		return nil, err
	}

	b.preExist = !isNew

	// Bring the interface up
	if err := upInterface(b.Name, false); err != nil {
		if err := ovsDelBridge(b.Name); err != nil {
			// Welp, we're boned
			log.Error("defunct bridge -- %v %v", b.Name, err)
		}

		return nil, err
	}

	return b, nil
}

// destroy a bridge with ovs, and remove all of the taps, etc associated with it
func (b *Bridge) Destroy() error {
	// first get all of the taps off of this bridge and destroy them
	for tap := range b.Taps {
		log.Debug("destroying tap %v", tap)
		if err := b.TapDestroy(tap); err != nil {
			log.Info("Destroy: could not destroy tap: %v", err)
		}
	}

	// don't destroy the bridge if it existed before we started
	if b.preExist {
		return nil
	}

	bridgeLock.Lock()
	defer bridgeLock.Unlock()

	b.Lock()
	defer b.Unlock()

	if err := ovsDelBridge(b.Name); err != nil {
		return err
	}

	delete(bridges, b.Name)
	return nil
}

// create and add a tap to a bridge. If a name is not provided, one will be
// automatically generated.
func (b *Bridge) TapCreate(name string, lan int, host bool) (tapName string, err error) {
	tapName = name
	if tapName == "" {
		tapName = <-tapNameChan
	}

	// Add the tap and only fail if it already exists and the caller did not
	// explicitly name it. If the caller did provide a name, assume that they
	// already created the tap for us.
	if err = addTap(tapName); err != nil && !(err == ErrAlreadyExists && name != "") {
		return
	}

	defer func() {
		// If there was an error, remove the tap. Again, handle the special
		// case where the caller provided the tap name explicitly by not
		// deleting the tap.
		if name == "" && err != nil {
			if err := delTap(tapName); err != nil {
				// Welp, we're boned
				log.Error("defunct tap -- %v %v", tapName, err)
			}
		}
	}()

	if err = upInterface(tapName, host); err != nil {
		return
	}

	// Host taps are brought up in promisc mode
	err = b.TapAdd(tapName, lan, host)
	return
}

// add a tap to the bridge
func (b *Bridge) TapAdd(tap string, lan int, host bool) (err error) {
	defer func() {
		if err == ErrAlreadyExists {
			// special case - we own the tap, but it already exists
			// on the bridge. simply remove and add it again.
			log.Info("tap %v is already on bridge, adding again", tap)
			if err = b.TapRemove(tap); err == nil {
				err = b.TapAdd(tap, lan, host)
			}
		}
	}()

	// start the ipmaclearner, if need be
	b.once.Do(b.startIML)

	b.Lock()
	defer b.Unlock()

	if _, ok := b.Taps[tap]; ok {
		return fmt.Errorf("tap is already connected to bridge: %v %v", b.Name, tap)
	}

	if err = ovsAddPort(b.Name, tap, lan, host); err != nil {
		return
	}

	b.Taps[tap] = Tap{
		lan:  lan,
		host: host,
	}

	return
}

// destroy and remove a tap from a bridge
func (b *Bridge) TapDestroy(tap string) error {
	if err := b.TapRemove(tap); err != nil {
		return err
	}

	return delTap(tap)
}

// remove a tap from a bridge
func (b *Bridge) TapRemove(tap string) error {
	b.Lock()
	defer b.Unlock()

	if err := ovsDelPort(b.Name, tap); err != nil {
		return err
	}

	delete(b.Taps, tap)
	return nil
}

// startIML starts the MAC listener.
func (b *Bridge) startIML() {
	// use openflow to redirect arp and icmp6 traffic to the local tap
	filters := []string{
		"dl_type=0x0806,actions=local,normal",
		"dl_type=0x86dd,nw_proto=58,icmp_type=135,actions=local,normal",
	}
	for _, filter := range filters {
		if err := addOpenflow(b.Name, filter); err != nil {
			log.Error("cannot start ip learner on bridge: %v", err)
			return
		}
	}

	iml, err := ipmac.NewLearner(b.Name)
	if err != nil {
		log.Error("cannot start ip learner on bridge: %v", err)
		return
	}

	b.iml = iml
}

// create a new netflow object for the specified bridge
func (b *Bridge) NewNetflow(timeout int) (*gonetflow.Netflow, error) {
	b.Lock()
	defer b.Unlock()

	if b.nf != nil {
		return nil, fmt.Errorf("bridge %v already has a netflow object", b.Name)
	}

	nf, port, err := gonetflow.NewNetflow()
	if err != nil {
		return nil, err
	}

	// connect openvswitch to our new netflow object
	args := []string{
		"--",
		"set",
		"Bridge",
		b.Name,
		"netflow=@nf",
		"--",
		"--id=@nf",
		"create",
		"NetFlow",
		fmt.Sprintf("targets=\"127.0.0.1:%v\"", port),
		fmt.Sprintf("active-timeout=%v", timeout),
	}

	if _, sErr, err := ovsCmdWrapper(args); err != nil {
		return nil, fmt.Errorf("NewNetflow: could not enable netflow: %v: %v", err, sErr)
	}

	b.nf = nf

	return nf, nil
}

// remove an active netflow object
func (b *Bridge) DestroyNetflow() error {
	b.Lock()
	defer b.Unlock()

	if b.nf == nil {
		return fmt.Errorf("bridge %v has no netflow object", b.Name)
	}

	b.nf.Stop()

	// disconnect openvswitch from netflow object
	args := []string{
		"clear",
		"Bridge",
		b.Name,
		"netflow",
	}

	if _, sErr, err := ovsCmdWrapper(args); err != nil {
		return fmt.Errorf("DestroyNetflow: %v: %v", err, sErr)
	}

	b.nf = nil

	return nil
}

// update the active timeout on a nf object
func (b *Bridge) UpdateNFTimeout(t int) error {
	b.Lock()
	defer b.Unlock()

	if b.nf == nil {
		return fmt.Errorf("bridge %v has no netflow object", b.Name)
	}

	args := []string{
		"set",
		"NetFlow",
		b.Name,
		fmt.Sprintf("active_timeout=%v", t),
	}
	if _, sErr, err := ovsCmdWrapper(args); err != nil {
		return fmt.Errorf("UpdateNFTimeout: %v: %v", err, sErr)
	}

	return nil
}

// add a vxlan or GRE tunnel to a bridge
func (b *Bridge) TunnelAdd(t int, remoteIP string) error {
	var tunnelType string
	switch t {
	case TYPE_VXLAN:
		tunnelType = "vxlan"
	case TYPE_GRE:
		tunnelType = "gre"
	default:
		return fmt.Errorf("invalid tunnel type: %v", t)
	}

	tapName := <-tapNameChan

	b.Lock()
	defer b.Unlock()

	args := []string{
		"add-port",
		b.Name,
		tapName,
		"--",
		"set",
		"interface",
		tapName,
		fmt.Sprintf("type=%v", tunnelType),
		fmt.Sprintf("options:remote_ip=%v", remoteIP),
	}
	if _, sErr, err := ovsCmdWrapper(args); err != nil {
		return fmt.Errorf("TunnelAdd: %v: %v", err, sErr)
	}

	b.Tunnel = append(b.Tunnel, tapName)

	return nil
}

// remove trunk port from a bridge
func (b *Bridge) TunnelRemove(iface string) error {
	// find this iface in the tunnel list
	index := -1
	for i, v := range b.Tunnel {
		if v == iface {
			index = i
			break
		}
	}
	if index == -1 {
		return fmt.Errorf("no tunnel port %v on bridge %v", iface, b.Name)
	}

	err := ovsDelPort(b.Name, b.Tunnel[index])
	if err == nil {
		b.Tunnel = append(b.Tunnel[:index], b.Tunnel[index+1:]...)
	}

	return err
}

// add an interface as a trunk port to a bridge
func (b *Bridge) TrunkAdd(iface string) error {
	b.Lock()
	defer b.Unlock()

	err := ovsAddPort(b.Name, iface, TrunkVLAN, false)
	if err == nil {
		b.Trunk = append(b.Trunk, iface)
	}

	return err
}

// remove trunk port from a bridge
func (b *Bridge) TrunkRemove(iface string) error {
	b.Lock()
	defer b.Unlock()

	// find this iface in the trunk list
	index := -1
	for i, v := range b.Trunk {
		if v == iface {
			index = i
			break
		}
	}
	if index == -1 {
		return fmt.Errorf("no trunk port %v on bridge %v", iface, b.Name)
	}

	err := ovsDelPort(b.Name, b.Trunk[index])
	if err == nil {
		b.Trunk = append(b.Trunk[:index], b.Trunk[index+1:]...)
	}

	return err
}

func (b *Bridge) MirrorAdd() (string, error) {
	// get a host tap
	tapName, err := hostTapCreate(b.Name, "none", "", 0)
	if err != nil {
		return "", err
	}

	// create the mirror for this bridge
	args := []string{
		"--",
		"--id=@p",
		"get",
		"port",
		tapName,
		"--",
		"--id=@m",
		"create",
		"mirror",
		"name=m0",
		"select-all=true",
		"output-port=@p",
		"--",
		"set",
		"bridge",
		b.Name,
		"mirrors=@m",
	}

	if _, sErr, err := ovsCmdWrapper(args); err != nil {
		return "", fmt.Errorf("openvswitch: %v: %v", err, sErr)
	}

	return tapName, nil
}

func (b *Bridge) MirrorRemove(tap string) error {
	// delete the mirror for this bridge
	args := []string{
		"clear",
		"bridge",
		b.Name,
		"mirrors",
	}

	if _, sErr, err := ovsCmdWrapper(args); err != nil {
		return fmt.Errorf("DeleteBridgeMirror: %v: %v", err, sErr)
	}

	// delete the associated host tap
	return hostTapDelete(tap)
}

// return a pointer to the specified bridge, creating it if it doesn't already
// exist. If b == "", return the default bridge
func getBridge(b string) (*Bridge, error) {
	if b == "" {
		b = DEFAULT_BRIDGE
	}

	bridgeLock.Lock()
	defer bridgeLock.Unlock()
	if v, ok := bridges[b]; ok {
		return v, nil
	}

	bridge, err := NewBridge(b)
	if err != nil {
		return nil, err
	}

	bridges[b] = bridge

	updateBridgeInfo()

	return bridge, nil
}

func enumerateBridges() []string {
	bridgeLock.Lock()
	defer bridgeLock.Unlock()
	var ret []string
	for k, _ := range bridges {
		ret = append(ret, k)
	}
	return ret
}

// return the netflow object of a current bridge
func getNetflowFromBridge(b string) (*gonetflow.Netflow, error) {
	bridgeLock.Lock()
	defer bridgeLock.Unlock()
	if v, ok := bridges[b]; ok {
		if v.nf == nil {
			return nil, fmt.Errorf("bridge %v has no netflow object", b)
		}
		return v.nf, nil
	} else {
		return nil, fmt.Errorf("no such bridge %v", b)
	}
}

// return a pointer to a bridge that has tap t attached to it, or error
func getBridgeFromTap(t string) (*Bridge, error) {
	log.Debugln("getBridgeFromTap")

	for k, b := range bridges {
		for tap, _ := range b.Taps {
			if tap == t {
				log.Debug("found tap %v in bridge %v", t, k)
				return b, nil
			}
		}
	}

	return nil, fmt.Errorf("tap %v not found", t)
}

// destroy all bridges
func bridgesDestroy() error {
	var errs []string
	for _, v := range bridges {
		if err := v.Destroy(); err != nil {
			errs = append(errs, err.Error())
		}
	}

	bridgeFile := filepath.Join(*f_base, "bridges")
	err := os.Remove(bridgeFile)
	if err != nil && !os.IsNotExist(err) {
		log.Error("bridgesDestroy: could not remove bridge file: %v", err)
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, " : "))
	}

	return nil
}

// called with bridgeLock set
func updateBridgeInfo() {
	log.Debugln("updateBridgeInfo")
	i := bridgeInfo()
	path := filepath.Join(*f_base, "bridges")
	err := ioutil.WriteFile(path, []byte(i), 0644)
	if err != nil {
		log.Fatalln(err)
	}
}

// return formatted bridge info. expected to be called with bridgeLock set
func bridgeInfo() string {
	if len(bridges) == 0 {
		return ""
	}

	var o bytes.Buffer
	w := new(tabwriter.Writer)
	w.Init(&o, 5, 0, 1, ' ', 0)
	fmt.Fprintf(w, "Bridge\tExisted before minimega\tActive VLANS\n")
	for _, v := range bridges {
		vlans := map[int]bool{}
		for _, tap := range v.Taps {
			vlans[tap.lan] = true
		}

		vlans2 := []int{}
		for k, _ := range vlans {
			vlans2 = append(vlans2, k)
		}
		sort.Ints(vlans2)

		fmt.Fprintf(w, "%v\t%v\t%v\n", v.Name, v.preExist, vlans2)
	}

	w.Flush()
	return o.String()
}

func hostTapCreate(bridge, ip, name string, lan int) (tapName string, err error) {
	var b *Bridge
	if b, err = getBridge(bridge); err != nil {
		return "", err
	}

	tapName = name
	if tapName == "" {
		tapName = <-tapNameChan
	}

	// Add the interface
	if err := b.TapAdd(tapName, lan, true); err != nil {
		return "", err
	}
	defer func() {
		// If there was an error, remove the tap. Again, handle the special
		// case where the caller provided the tap name explicitly by not
		// deleting the tap.
		if name == "" && err != nil {
			if err := b.TapRemove(tapName); err != nil {
				// Welp, we're boned
				log.Error("defunct tap -- %v %v", tapName, err)
			}
		}
	}()

	if err := upInterface(tapName, true); err != nil {
		return "", err
	}

	if strings.ToLower(ip) == "none" {
		return tapName, nil
	} else if strings.ToLower(ip) == "dhcp" {
		var sErr bytes.Buffer

		cmd := exec.Command(process("dhcp"), tapName)
		cmd.Stderr = &sErr
		log.Debug("obtaining dhcp on tap %v", tapName)

		if err = cmd.Run(); err != nil {
			return "", fmt.Errorf("%v: %v", err, sErr.String())
		}
	} else {
		// Must be a static IP
		var sErr bytes.Buffer

		cmd := exec.Command(process("ip"), "addr", "add", "dev", tapName, ip)
		cmd.Stderr = &sErr
		log.Debug("setting ip on tap %v", tapName)

		if err = cmd.Run(); err != nil {
			return "", fmt.Errorf("%v: %v", err, sErr.String())
		}
	}

	return tapName, nil
}

func hostTapList(resp *minicli.Response) {
	resp.Header = []string{"bridge", "tap", "vlan"}
	resp.Tabular = [][]string{}

	// find all the host taps first
	for k, b := range bridges {
		for name, tap := range b.Taps {
			if tap.host {
				resp.Tabular = append(resp.Tabular, []string{
					k, name, strconv.Itoa(tap.lan),
				})
			}
		}
	}
}

func hostTapDelete(tap string) error {
	var c []*Bridge
	// special case, *, which should delete all host taps from all bridges
	if tap == Wildcard {
		for _, v := range bridges {
			c = append(c, v)
		}
	} else {
		b, err := getBridgeFromTap(tap)
		if err != nil {
			return err
		}
		c = append(c, b)
	}
	for _, b := range c {
		if tap == Wildcard {
			// remove all host taps on this vlan
			for name, t := range b.Taps {
				if t.host {
					b.TapDestroy(name)
				}
			}
			continue
		}
		if t, ok := b.Taps[tap]; ok {
			if !t.host {
				return fmt.Errorf("not a host tap")
			}
			b.TapDestroy(tap)
		}
	}
	return nil
}

// upInterface activates an interface parameter using the `ip` command. promisc
// controls whether the interface is brought up in promiscuous mode.
func upInterface(name string, promisc bool) error {
	args := []string{"link", "set", name, "up"}
	if promisc {
		args = append(args, "promisc", "on")
	}

	if _, sErr, err := cmdWrapper(process("ip"), args...); err != nil {
		return fmt.Errorf("ip: %v: %v", err, sErr)
	}

	return nil
}

// downInterface deactivates an interface parameter using the `ip` command.
func downInterface(name string) error {
	_, sErr, err := cmdWrapper(process("ip"), "link", "set", name, "down")
	if err != nil {
		return fmt.Errorf("ip: %v: %v", err, sErr)
	}

	return nil
}

// createTap adds a tuntap based on the add parameter using the `ip` command.
func addTap(name string) error {
	_, sErr, err := cmdWrapper(process("ip"), "tuntap", "add", "mode", "tap", name)
	if strings.Contains(sErr, "Device or resource busy") {
		return ErrAlreadyExists
	} else if err != nil {
		return fmt.Errorf("ip: %v: %v", err, sErr)
	}

	return nil
}

// delTap removes a tuntap based on the add parameter using the `ip` command.
func delTap(name string) error {
	_, sErr, err := cmdWrapper(process("ip"), "tuntap", "del", "mode", "tap", name)
	if err != nil {
		return fmt.Errorf("ip: %v: %v", err, sErr)
	}

	return nil
}

func addOpenflow(bridge, filter string) error {
	ovsLock.Lock()
	defer ovsLock.Unlock()

	_, sErr, err := cmdWrapper(process("openflow"), "add-flow", bridge, filter)
	if err != nil {
		return fmt.Errorf("openflow: %v: %v", err, sErr)
	}

	return nil
}

// create and add a veth tap to a bridge
func (b *Bridge) ContainerTapCreate(lan int, ns string, mac string, index int) (string, error) {
	tapName := <-tapNameChan

	var sOut bytes.Buffer
	var sErr bytes.Buffer

	p := process("ip")
	cmd := &exec.Cmd{
		Path: p,
		Args: []string{
			p,
			"link",
			"add",
			tapName,
			"type",
			"veth",
			"peer",
			"mega", // does the namespace ignore this?
			"netns",
			ns,
		},
		Env:    nil,
		Dir:    "",
		Stdout: &sOut,
		Stderr: &sErr,
	}
	log.Debug("creating tap with cmd: %v", cmd)
	err := cmd.Run()
	if err != nil {
		e := fmt.Errorf("ip: %v: %v", err, sErr.String())
		return "", e
	}

	// Add the interface
	if err := b.TapAdd(tapName, lan, false); err != nil {
		return "", err
	}
	defer func() {
		// If there was an error, remove the tap. Again, handle the special
		// case where the caller provided the tap name explicitly by not
		// deleting the tap.
		if err != nil {
			if err := b.TapRemove(tapName); err != nil {
				// Welp, we're boned
				log.Error("defunct tap -- %v %v", tapName, err)
			}
		}
	}()

	if err := upInterface(tapName, false); err != nil {
		return "", err
	}

	cmd = &exec.Cmd{
		Path: p,
		Args: []string{
			p,
			"netns",
			"exec",
			ns,
			"ip",
			"link",
			"set",
			"dev",
			fmt.Sprintf("veth%v", index),
			"address",
			mac,
		},
		Env:    nil,
		Dir:    "",
		Stdout: &sOut,
		Stderr: &sErr,
	}
	log.Debug("setting container mac address with cmd: %v", cmd)
	err = cmd.Run()
	if err != nil {
		e := fmt.Errorf("ip: %v: %v", err, sErr.String())
		return "", e
	}
	return tapName, nil
}

// destroy and remove a container tap from a bridge
func (b *Bridge) ContainerTapDestroy(lan int, tap string) error {
	err := b.TapRemove(tap)
	if err != nil {
		log.Info("TapDestroy: could not remove tap: %v", err)
	}

	if err := downInterface(tap); err != nil {
		return err
	}

	var sOut bytes.Buffer
	var sErr bytes.Buffer

	p := process("ip")
	cmd := &exec.Cmd{
		Path: p,
		Args: []string{
			p,
			"link",
			"del",
			tap,
			"type",
			"veth",
			"peer",
			"eth0",
		},
		Env:    nil,
		Dir:    "",
		Stdout: &sOut,
		Stderr: &sErr,
	}
	log.Debug("destroying tap with cmd: %v", cmd)
	err = cmd.Run()
	if err != nil {
		e := fmt.Errorf("ip: %v: %v", err, sErr.String())
		return e
	}
	return nil
}
