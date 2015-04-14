// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	log "minilog"
	"os"
	"os/exec"
	"path/filepath"
	"qmp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

type KVMConfig struct {
	Append      string
	CdromPath   string
	InitrdPath  string
	KernelPath  string
	MigratePath string
	UUID        string

	Snapshot bool

	DiskPaths  []string
	QemuAppend []string // extra arguments for QEMU
}

type vmKVM struct {
	vmBase    // embed
	KVMConfig // embed

	// Internal variables
	hotplug      map[int]string
	instancePath string
	kill         chan bool // kill channel to signal to shut a vm down

	pid int
	q   qmp.Conn // qmp connection for this vm
}

type qemuOverride struct {
	match string
	repl  string
}

var (
	kvmConfig *KVMConfig // current kvm config, updated by CLI

	QemuOverrides      map[int]*qemuOverride
	qemuOverrideIdChan chan int

	savedInfo map[string]*KVMConfig
)

// Ensure that vmKVM implements the VM interface
var _ VM = (*vmKVM)(nil)

func init() {
	kvmConfig = &KVMConfig{}

	savedInfo = make(map[string]*KVMConfig)
	QemuOverrides = make(map[int]*qemuOverride)
	qemuOverrideIdChan = makeIDChan()

	// Reset everything to default
	for _, fns := range kvmConfigFns {
		fns.Clear(kvmConfig)
	}
}

// Copy makes a deep copy and returns reference to the new struct.
func (old *KVMConfig) Copy() *KVMConfig {
	res := new(KVMConfig)

	// Copy all fields
	*res = *old

	// Make deep copy of slices
	res.DiskPaths = make([]string, len(old.DiskPaths))
	copy(res.DiskPaths, old.DiskPaths)
	res.QemuAppend = make([]string, len(old.QemuAppend))
	copy(res.QemuAppend, old.QemuAppend)

	return res
}

func (vm *vmKVM) Config() *VMConfig {
	return &vm.VMConfig
}

func NewKVM() *vmKVM {
	vm := new(vmKVM)

	vm.vmBase = *NewVM()

	vm.kill = make(chan bool)
	vm.hotplug = make(map[int]string)

	return vm
}

// launch one or more vms. this will copy the info struct, one per vm and
// launch each one in a goroutine. it will not return until all vms have
// reported that they've launched.
func (vm *vmKVM) Launch(name string, ack chan int) error {
	if err := vm.vmBase.launch(name); err != nil {
		return err
	}
	vm.KVMConfig = *kvmConfig.Copy() // deep-copy configured fields

	vmLock.Lock()
	vms[vm.id] = vm
	vmLock.Unlock()

	go vm.launch(ack)

	return nil
}

func (vm *vmKVM) Start() error {
	s := vm.State()

	stateMask := VM_PAUSED | VM_BUILDING | VM_QUIT
	if s&stateMask == 0 {
		return nil
	}

	if s == VM_QUIT {
		log.Info("restarting VM: %v", vm.id)
		ack := make(chan int)
		go vm.launch(ack)
		log.Debugln("ack restarted VM %v", <-ack)
	}

	log.Info("starting VM: %v", vm.id)
	err := vm.q.Start()
	if err != nil {
		vm.setState(VM_ERROR)
	} else {
		vm.setState(VM_RUNNING)
	}

	return err
}

func (vm *vmKVM) Stop() error {
	if vm.State() != VM_RUNNING {
		return fmt.Errorf("VM %v not running", vm.id)
	}

	log.Info("stopping VM: %v", vm.id)
	err := vm.q.Stop()
	if err == nil {
		vm.setState(VM_PAUSED)
	}

	return err
}

func (vm *vmKVM) Kill() error {
	vm.kill <- true
	// TODO: ACK if killed?
	return nil
}

func (vm *vmKVM) String() string {
	return fmt.Sprintf("%s:%d:kvm", hostname, vm.id)
}

func (vm *vmKVM) Info(masks []string) ([]string, error) {
	res := make([]string, 0, len(masks))

	for _, mask := range masks {
		// If it's a field handled by the baseVM, use it.
		if v, err := vm.vmBase.info(mask); err == nil {
			res = append(res, v)
			continue
		}

		// If it's a configurable field, use the Print fn.
		if fns, ok := kvmConfigFns[mask]; ok {
			res = append(res, fns.Print(&vm.KVMConfig))
			continue
		}

		switch mask {
		case "type":
			res = append(res, "kvm")
		case "cc_active":
			// TODO: This won't work if it's being run from a different host...
			activeClients := ccClients()
			res = append(res, fmt.Sprintf("%v", activeClients[vm.UUID]))
		default:
			return nil, fmt.Errorf("invalid mask: %s", mask)
		}
	}

	return res, nil
}

func (vm *KVMConfig) configToString() string {
	// create output
	var o bytes.Buffer
	w := new(tabwriter.Writer)
	w.Init(&o, 5, 0, 1, ' ', 0)
	fmt.Fprintln(&o, "Current VM configuration:")
	//fmt.Fprintf(w, "Memory:\t%v\n", vm.Memory)
	//fmt.Fprintf(w, "VCPUS:\t%v\n", vm.Vcpus)
	fmt.Fprintf(w, "Migrate Path:\t%v\n", vm.MigratePath)
	fmt.Fprintf(w, "Disk Paths:\t%v\n", vm.DiskPaths)
	fmt.Fprintf(w, "CDROM Path:\t%v\n", vm.CdromPath)
	fmt.Fprintf(w, "Kernel Path:\t%v\n", vm.KernelPath)
	fmt.Fprintf(w, "Initrd Path:\t%v\n", vm.InitrdPath)
	fmt.Fprintf(w, "Kernel Append:\t%v\n", vm.Append)
	fmt.Fprintf(w, "QEMU Path:\t%v\n", process("qemu"))
	fmt.Fprintf(w, "QEMU Append:\t%v\n", vm.QemuAppend)
	fmt.Fprintf(w, "Snapshot:\t%v\n", vm.Snapshot)
	//fmt.Fprintf(w, "Networks:\t%v\n", vm.NetworkString())
	fmt.Fprintf(w, "UUID:\t%v\n", vm.UUID)
	w.Flush()
	return o.String()
}

func (vm *vmKVM) QMPRaw(input string) (string, error) {
	return vm.q.Raw(input)
}

func (vm *vmKVM) Migrate(filename string) error {
	path := filepath.Join(*f_iomBase, filename)
	return vm.q.MigrateDisk(path)
}

func (vm *vmKVM) QueryMigrate() (string, float64, error) {
	var status string
	var completed float64

	r, err := vm.q.QueryMigrate()
	if err != nil {
		return "", 0.0, err
	}

	// find the status
	if s, ok := r["status"]; ok {
		status = s.(string)
	} else {
		return status, completed, fmt.Errorf("could not decode status: %v", r)
	}

	var ram map[string]interface{}
	switch status {
	case "completed":
		completed = 100.0
		return status, completed, nil
	case "failed":
		return status, completed, nil
	case "active":
		if e, ok := r["ram"]; !ok {
			return status, completed, fmt.Errorf("could not decode ram segment: %v", e)
		} else {
			switch e.(type) {
			case map[string]interface{}:
				ram = e.(map[string]interface{})
			default:
				return status, completed, fmt.Errorf("invalid ram type: %v", e)
			}
		}
	}

	total := ram["total"].(float64)
	transferred := ram["transferred"].(float64)

	if total == 0.0 {
		return status, completed, fmt.Errorf("zero total ram!")
	}

	completed = transferred / total

	return status, completed, nil
}

func (vm *vmKVM) launchPreamble(ack chan int) bool {
	// check if the vm has a conflict with the disk or mac address of another vm
	// build state of currently running system
	macMap := map[string]bool{}
	selfMacMap := map[string]bool{}
	diskSnapshotted := map[string]bool{}
	diskPersistent := map[string]bool{}

	vmLock.Lock()
	defer vmLock.Unlock()

	vm.instancePath = *f_base + strconv.Itoa(vm.id) + "/"
	err := os.MkdirAll(vm.instancePath, os.FileMode(0700))
	if err != nil {
		log.Errorln(err)
		teardown()
	}

	// generate a UUID if we don't have one
	if vm.UUID == "" {
		vm.UUID = generateUUID()
	}

	// populate selfMacMap
	for _, net := range vm.Networks {
		if net.MAC == "" { // don't worry about empty mac addresses
			continue
		}

		if _, ok := selfMacMap[net.MAC]; ok {
			// if this vm specified the same mac address for two interfaces
			log.Errorln("Cannot specify the same mac address for two interfaces")
			vm.setState(VM_ERROR)
			ack <- vm.id // signal that this vm is "done" launching
			return false
		}
		selfMacMap[net.MAC] = true
	}

	stateMask := VM_BUILDING | VM_RUNNING | VM_PAUSED

	// populate macMap, diskSnapshotted, and diskPersistent
	for _, vm2 := range vms {
		if vm == vm2 { // ignore this vm
			continue
		}

		s := vm2.State()

		if s&stateMask != 0 {
			// populate mac addresses set
			for _, net := range vm2.Config().Networks {
				macMap[net.MAC] = true
			}

			// TODO: Check non-kvm as well?
			if vm2, ok := vm2.(*vmKVM); ok {
				// populate disk sets
				if len(vm2.DiskPaths) != 0 {
					for _, diskpath := range vm2.DiskPaths {
						if vm2.Snapshot {
							diskSnapshotted[diskpath] = true
						} else {
							diskPersistent[diskpath] = true
						}
					}
				}
			}
		}
	}

	// check for mac address conflicts and fill in unspecified mac addresses without conflict
	for i, net := range vm.Networks {
		if net.MAC == "" { // create mac addresses where unspecified
			existsOther, existsSelf, newMac := true, true, "" // entry condition/initialization
			for existsOther || existsSelf {                   // loop until we generate a random mac that doesn't conflict (already exist)
				newMac = randomMac()               // generate a new mac address
				_, existsOther = macMap[newMac]    // check it against the set of mac addresses from other vms
				_, existsSelf = selfMacMap[newMac] // check it against the set of mac addresses specified from this vm
			}

			vm.Networks[i].MAC = newMac // set the unspecified mac address
			selfMacMap[newMac] = true   // add this mac to the set of mac addresses for this vm
		}
	}

	// check for disk conflict
	for _, diskPath := range vm.DiskPaths {
		_, existsSnapshotted := diskSnapshotted[diskPath]                    // check if another vm is using this disk in snapshot mode
		_, existsPersistent := diskPersistent[diskPath]                      // check if another vm is using this disk in persistent mode (snapshot=false)
		if existsPersistent || (vm.Snapshot == false && existsSnapshotted) { // if we have a disk conflict
			log.Error("disk path %v is already in use by another vm.", diskPath)
			vm.setState(VM_ERROR)
			ack <- vm.id
			return false
		}
	}

	return true
}

func (vm *vmKVM) launch(ack chan int) {
	log.Info("launching vm: %v", vm.id)

	s := vm.State()

	// don't repeat the preamble if we're just in the quit state
	if s != VM_QUIT && !vm.launchPreamble(ack) {
		return
	}

	vm.setState(VM_BUILDING)

	// write the config for this vm
	config := vm.configToString()
	err := ioutil.WriteFile(vm.instancePath+"config", []byte(config), 0664)
	if err != nil {
		log.Errorln(err)
		teardown()
	}
	err = ioutil.WriteFile(vm.instancePath+"name", []byte(vm.name), 0664)
	if err != nil {
		log.Errorln(err)
		teardown()
	}

	var args []string
	var sOut bytes.Buffer
	var sErr bytes.Buffer
	var cmd *exec.Cmd
	var waitChan = make(chan int)

	// clear taps, we may have come from the quit state
	for i := range vm.Networks {
		vm.Networks[i].Tap = ""
	}

	// create and add taps if we are associated with any networks
	for i, net := range vm.Networks {
		b, err := getBridge(net.Bridge)
		if err != nil {
			log.Error("get bridge: %v", err)
			vm.setState(VM_ERROR)
			ack <- vm.id
			return
		}

		tap, err := b.TapCreate(net.VLAN)
		if err != nil {
			log.Error("create tap: %v", err)
			vm.setState(VM_ERROR)
			ack <- vm.id
			return
		}

		vm.Networks[i].Tap = tap
	}

	if len(vm.Networks) > 0 {
		taps := []string{}
		for _, net := range vm.Networks {
			taps = append(taps, net.Tap)
		}

		err := ioutil.WriteFile(vm.instancePath+"taps", []byte(strings.Join(taps, "\n")), 0666)
		if err != nil {
			log.Error("write instance taps file: %v", err)
			vm.setState(VM_ERROR)
			ack <- vm.id
			return
		}
	}

	args = vm.vmGetArgs(true)
	args = ParseQemuOverrides(args)
	log.Debug("final qemu args: %#v", args)

	cmd = &exec.Cmd{
		Path:   process("qemu"),
		Args:   args,
		Env:    nil,
		Dir:    "",
		Stdout: &sOut,
		Stderr: &sErr,
	}
	err = cmd.Start()
	if err != nil {
		log.Error("start qemu: %v %v", err, sErr.String())
		vm.setState(VM_ERROR)
		ack <- vm.id
		return
	}

	vm.pid = cmd.Process.Pid
	log.Debug("vm %v has pid %v", vm.id, vm.pid)

	vm.CheckAffinity()

	go func() {
		err := cmd.Wait()
		vm.setState(VM_QUIT)
		if err != nil {
			if err.Error() != "signal: killed" { // because we killed it
				log.Error("kill qemu: %v %v", err, sErr.String())
				vm.setState(VM_ERROR)
			}
		}
		waitChan <- vm.id
	}()

	// we can't just return on error at this point because we'll leave dangling goroutines, we have to clean up on failure
	sendKillAck := false

	// connect to qmp
	connected := false
	for count := 0; count < QMP_CONNECT_RETRY; count++ {
		vm.q, err = qmp.Dial(vm.qmpPath())
		if err == nil {
			connected = true
			break
		}
		time.Sleep(QMP_CONNECT_DELAY * time.Millisecond)
	}

	if !connected {
		log.Error("vm %v failed to connect to qmp: %v", vm.id, err)
		vm.setState(VM_ERROR)
		cmd.Process.Kill()
		<-waitChan
		ack <- vm.id
	} else {
		go vm.asyncLogger()

		ack <- vm.id

		select {
		case <-waitChan:
			log.Info("VM %v exited", vm.id)
		case <-vm.kill:
			log.Info("Killing VM %v", vm.id)
			cmd.Process.Kill()
			<-waitChan
			sendKillAck = true // wait to ack until we've cleaned up
		}
	}

	for _, net := range vm.Networks {
		b, err := getBridge(net.Bridge)
		if err != nil {
			log.Error("get bridge: %v", err)
		} else {
			b.TapDestroy(net.VLAN, net.Tap)
		}
	}

	if sendKillAck {
		killAck <- vm.id
	}
}

// update the vm state, and write the state to file
func (vm *vmKVM) setState(s VMState) {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	vm.state = s
	err := ioutil.WriteFile(vm.instancePath+"state", []byte(s.String()), 0666)
	if err != nil {
		log.Error("write instance state file: %v", err)
	}
}

// return the path to the qmp socket
func (vm *vmKVM) qmpPath() string {
	return vm.instancePath + "qmp"
}

// build the horribly long qemu argument string
func (vm *vmKVM) vmGetArgs(commit bool) []string {
	var args []string

	sId := strconv.Itoa(vm.id)

	args = append(args, process("qemu"))

	args = append(args, "-enable-kvm")

	args = append(args, "-name")
	args = append(args, sId)

	args = append(args, "-m")
	args = append(args, vm.Memory)

	args = append(args, "-nographic")

	args = append(args, "-balloon")
	args = append(args, "none")

	args = append(args, "-vnc")
	args = append(args, "0.0.0.0:"+sId) // if we have more than 10000 vnc sessions, we're in trouble

	args = append(args, "-usbdevice") // this allows absolute pointers in vnc, and works great on android vms
	args = append(args, "tablet")

	args = append(args, "-smp")
	args = append(args, vm.Vcpus)

	args = append(args, "-qmp")
	args = append(args, "unix:"+vm.qmpPath()+",server")

	args = append(args, "-vga")
	args = append(args, "cirrus")

	args = append(args, "-rtc")
	args = append(args, "clock=vm,base=utc")

	args = append(args, "-device")
	args = append(args, "virtio-serial")

	args = append(args, "-chardev")
	args = append(args, "socket,id=charserial0,path="+vm.instancePath+"serial,server,nowait")

	args = append(args, "-device")
	args = append(args, "virtserialport,chardev=charserial0,id=serial0,name=serial0")

	args = append(args, "-pidfile")
	args = append(args, vm.instancePath+"qemu.pid")

	args = append(args, "-k")
	args = append(args, "en-us")

	args = append(args, "-cpu")
	args = append(args, "host")

	args = append(args, "-net")
	args = append(args, "none")

	args = append(args, "-S")

	if vm.MigratePath != "" {
		args = append(args, "-incoming")
		args = append(args, fmt.Sprintf("exec:cat %v", vm.MigratePath))
	}

	if len(vm.DiskPaths) != 0 {
		for _, diskPath := range vm.DiskPaths {
			args = append(args, "-drive")
			args = append(args, "file="+diskPath+",media=disk")
		}
	}

	if vm.Snapshot {
		args = append(args, "-snapshot")
	}

	if vm.KernelPath != "" {
		args = append(args, "-kernel")
		args = append(args, vm.KernelPath)
	}
	if vm.InitrdPath != "" {
		args = append(args, "-initrd")
		args = append(args, vm.InitrdPath)
	}
	if vm.Append != "" {
		args = append(args, "-append")
		args = append(args, vm.Append)
	}

	if vm.CdromPath != "" {
		args = append(args, "-drive")
		args = append(args, "file="+vm.CdromPath+",if=ide,index=1,media=cdrom")
		args = append(args, "-boot")
		args = append(args, "once=d")
	}

	bus := 1
	addr := 1
	args = append(args, fmt.Sprintf("-device"))
	args = append(args, fmt.Sprintf("pci-bridge,id=pci.%v,chassis_nr=%v", bus, bus))
	for _, net := range vm.Networks {
		args = append(args, "-netdev")
		args = append(args, fmt.Sprintf("tap,id=%v,script=no,ifname=%v", net.Tap, net.Tap))
		args = append(args, "-device")
		if commit {
			b, err := getBridge(net.Bridge)
			if err != nil {
				log.Error("get bridge: %v", err)
			}
			b.iml.AddMac(net.MAC)
		}
		args = append(args, fmt.Sprintf("driver=%v,netdev=%v,mac=%v,bus=pci.%v,addr=0x%x", net.Driver, net.Tap, net.MAC, bus, addr))
		addr++
		if addr == 32 {
			addr = 1
			bus++
			args = append(args, fmt.Sprintf("-device"))
			args = append(args, fmt.Sprintf("pci-bridge,id=pci.%v,chassis_nr=%v", bus, bus))
		}
	}

	// hook for hugepage support
	if hugepagesMountPath != "" {
		args = append(args, "-mem-info")
		args = append(args, hugepagesMountPath)
	}

	if len(vm.QemuAppend) > 0 {
		args = append(args, vm.QemuAppend...)
	}

	args = append(args, "-uuid")
	args = append(args, vm.UUID)

	log.Info("args for vm %v is: %#v", vm.id, args)
	return args
}

// log any asynchronous messages, such as vnc connects, to log.Info
func (vm *vmKVM) asyncLogger() {
	for {
		v := vm.q.Message()
		if v == nil {
			return
		}
		log.Info("VM %v received asynchronous message: %v", vm.id, v)
	}
}

func (vm *vmKVM) hotplugRemove(id int) error {
	hid := fmt.Sprintf("hotplug%v", id)
	log.Debugln("hotplug id:", hid)
	if _, ok := vm.hotplug[id]; !ok {
		return errors.New("no such hotplug device id")
	}

	resp, err := vm.q.USBDeviceDel(hid)
	if err != nil {
		return err
	}

	log.Debugln("hotplug usb device del response:", resp)
	resp, err = vm.q.DriveDel(hid)
	if err != nil {
		return err
	}

	log.Debugln("hotplug usb drive del response:", resp)
	delete(vm.hotplug, id)
	return nil
}

func (vm *vmKVM) info(masks []string) ([]string, error) {
	res := make([]string, 0, len(masks))

	for _, mask := range masks {
		switch mask {
		case "id":
			res = append(res, fmt.Sprintf("%v", vm.ID))
		case "name":
			res = append(res, fmt.Sprintf("%v", vm.Name))
		case "memory":
			res = append(res, fmt.Sprintf("%v", vm.Memory))
		case "vcpus":
			res = append(res, fmt.Sprintf("%v", vm.Vcpus))
		case "state":
			res = append(res, vm.state.String())
		case "migrate":
			res = append(res, fmt.Sprintf("%v", vm.MigratePath))
		case "disk":
			res = append(res, fmt.Sprintf("%v", vm.DiskPaths))
		case "snapshot":
			res = append(res, fmt.Sprintf("%v", vm.Snapshot))
		case "initrd":
			res = append(res, fmt.Sprintf("%v", vm.InitrdPath))
		case "kernel":
			res = append(res, fmt.Sprintf("%v", vm.KernelPath))
		case "cdrom":
			res = append(res, fmt.Sprintf("%v", vm.CdromPath))
		case "append":
			res = append(res, fmt.Sprintf("%v", vm.Append))
		case "bridge":
			vals := []string{}
			for _, net := range vm.Networks {
				vals = append(vals, net.Bridge)
			}
			res = append(res, fmt.Sprintf("%v", vals))
		case "tap":
			vals := []string{}
			for _, net := range vm.Networks {
				vals = append(vals, net.Tap)
			}
			res = append(res, fmt.Sprintf("%v", vals))
		case "mac":
			vals := []string{}
			for _, net := range vm.Networks {
				vals = append(vals, net.MAC)
			}
			res = append(res, fmt.Sprintf("%v", vals))
		case "bandwidth":
			var bw []string
			bandwidthLock.Lock()
			for _, net := range vm.Networks {
				t := bandwidthStats[net.Tap]
				if t == nil {
					bw = append(bw, "0.0/0.0")
				} else {
					bw = append(bw, fmt.Sprintf("%v", t))
				}
			}
			bandwidthLock.Unlock()
			res = append(res, fmt.Sprintf("%v", bw))
		case "tags":
			res = append(res, fmt.Sprintf("%v", vm.Tags))
		case "ip":
			var ips []string
			for _, net := range vm.Networks {
				// TODO: This won't work if it's being run from a different host...
				b, err := getBridge(net.Bridge)
				if err != nil {
					log.Errorln(err)
					continue
				}

				ip := b.GetIPFromMac(net.MAC)
				if ip != nil {
					ips = append(ips, ip.IP4)
				}
			}
			res = append(res, fmt.Sprintf("%v", ips))
		case "ip6":
			var ips []string
			for _, net := range vm.Networks {
				// TODO: This won't work if it's being run from a different host...
				b, err := getBridge(net.Bridge)
				if err != nil {
					log.Errorln(err)
					continue
				}

				ip := b.GetIPFromMac(net.MAC)
				if ip != nil {
					ips = append(ips, ip.IP6)
				}
			}
			res = append(res, fmt.Sprintf("%v", ips))
		case "vlan":
			var vlans []string
			for _, net := range vm.Networks {
				if net.VLAN == -1 {
					vlans = append(vlans, "disconnected")
				} else {
					vlans = append(vlans, fmt.Sprintf("%v", net.VLAN))
				}
			}
			res = append(res, fmt.Sprintf("%v", vlans))
		case "uuid":
			res = append(res, fmt.Sprintf("%v", vm.UUID))
		case "cc_active":
			// TODO: This won't work if it's being run from a different host...
			activeClients := ccClients()
			res = append(res, fmt.Sprintf("%v", activeClients[vm.UUID]))
		default:
			return nil, fmt.Errorf("invalid mask: %s", mask)
		}
	}

	return res, nil
}

func qemuOverrideString() string {
	// create output
	var o bytes.Buffer
	w := new(tabwriter.Writer)
	w.Init(&o, 5, 0, 1, ' ', 0)
	fmt.Fprintln(&o, "id\tmatch\treplacement")
	for i, v := range QemuOverrides {
		fmt.Fprintf(&o, "%v\t\"%v\"\t\"%v\"\n", i, v.match, v.repl)
	}
	w.Flush()

	// TODO
	args := []string{} //vm.vmGetArgs(false)
	preArgs := unescapeString(args)
	postArgs := strings.Join(ParseQemuOverrides(args), " ")

	r := o.String()
	r += fmt.Sprintf("\nBefore overrides:\n%v\n", preArgs)
	r += fmt.Sprintf("\nAfter overrides:\n%v\n", postArgs)

	return r
}

func delVMQemuOverride(arg string) error {
	if arg == Wildcard {
		QemuOverrides = make(map[int]*qemuOverride)
		return nil
	}

	id, err := strconv.Atoi(arg)
	if err != nil {
		return fmt.Errorf("invalid id %v", arg)
	}

	delete(QemuOverrides, id)
	return nil
}

func addVMQemuOverride(match, repl string) error {
	id := <-qemuOverrideIdChan

	QemuOverrides[id] = &qemuOverride{
		match: match,
		repl:  repl,
	}

	return nil
}

func ParseQemuOverrides(input []string) []string {
	ret := unescapeString(input)
	for _, v := range QemuOverrides {
		ret = strings.Replace(ret, v.match, v.repl, -1)
	}
	return fieldsQuoteEscape("\"", ret)
}