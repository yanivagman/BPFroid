package tracee

import (
	"bufio"
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	bpf "github.com/aquasecurity/tracee/libbpfgo"
	"github.com/aquasecurity/tracee/tracee/external"
)

// TraceeConfig is a struct containing user defined configuration of tracee
type TraceeConfig struct {
	Filter             *Filter
	Capture            *CaptureConfig
	Output             *OutputConfig
	PerfBufferSize     int
	BlobPerfBufferSize int
	SecurityAlerts     bool
	ApiHookConfigs        []apiHookConfig
	LibHookConfigs        []libHookConfig
	maxPidsCache       int // maximum number of pids to cache per mnt ns (in Tracee.pidsInMntns)
	BPFObjPath         string
}

type Filter struct {
	EventsToTrace []int32
	UIDFilter     *UintFilter
	PIDFilter     *UintFilter
	NewPidFilter  *BoolFilter
	MntNSFilter   *UintFilter
	PidNSFilter   *UintFilter
	UTSFilter     *StringFilter
	CommFilter    *StringFilter
	ContFilter    *BoolFilter
	NewContFilter *BoolFilter
	RetFilter     *RetFilter
	ArgFilter     *ArgFilter
	Follow        bool
}

type UintFilter struct {
	Equal    []uint64
	NotEqual []uint64
	Greater  uint64
	Less     uint64
	Is32Bit  bool
	Enabled  bool
}

type IntFilter struct {
	Equal    []int64
	NotEqual []int64
	Greater  int64
	Less     int64
	Is32Bit  bool
	Enabled  bool
}

type StringFilter struct {
	Equal    []string
	NotEqual []string
	Enabled  bool
}

type BoolFilter struct {
	Value   bool
	Enabled bool
}

type RetFilter struct {
	Filters map[int32]IntFilter
	Enabled bool
}

type ArgFilter struct {
	Filters map[int32]map[string]ArgFilterVal // key to the first map is event id, and to the second map the argument name
	Enabled bool
}

type ArgFilterVal struct {
	argTag   argTag
	Equal    []string
	NotEqual []string
}

type CaptureConfig struct {
	OutputPath      string
	FileWrite       bool
	FilterFileWrite []string
	Exec            bool
	Mem             bool
}

type OutputConfig struct {
	Format         string
	OutPath        string
	ErrPath        string
	EOT            bool
	StackAddresses bool
	DetectSyscall  bool
	ExecEnv        bool
}

// Validate does static validation of the configuration
func (cfg OutputConfig) Validate() error {
	if cfg.Format != "table" && cfg.Format != "table-verbose" && cfg.Format != "json" && cfg.Format != "gob" && !strings.HasPrefix(cfg.Format, "gotemplate=") {
		return fmt.Errorf("unrecognized output format: %s", cfg.Format)
	}
	return nil
}

// Validate does static validation of the configuration
func (tc TraceeConfig) Validate() error {
	if tc.Filter.EventsToTrace == nil {
		return fmt.Errorf("eventsToTrace is nil")
	}

	for _, e := range tc.Filter.EventsToTrace {
		if _, ok := EventsIDToEvent[e]; !ok {
			return fmt.Errorf("invalid event to trace: %d", e)
		}
	}
	for eventID, eventFilters := range tc.Filter.ArgFilter.Filters {
		for argName := range eventFilters {
			eventParams, ok := EventsIDToParams[eventID]
			if !ok {
				return fmt.Errorf("invalid argument filter event id: %d", eventID)
			}
			// check if argument name exists for this event
			argFound := false
			for i := range eventParams {
				if eventParams[i].Name == argName {
					argFound = true
					break
				}
			}
			if !argFound {
				return fmt.Errorf("invalid argument filter argument name: %s", argName)
			}
		}
	}
	if (tc.PerfBufferSize & (tc.PerfBufferSize - 1)) != 0 {
		return fmt.Errorf("invalid perf buffer size - must be a power of 2")
	}
	if (tc.BlobPerfBufferSize & (tc.BlobPerfBufferSize - 1)) != 0 {
		return fmt.Errorf("invalid perf buffer size - must be a power of 2")
	}
	if len(tc.Capture.FilterFileWrite) > 3 {
		return fmt.Errorf("too many file-write filters given")
	}
	for _, filter := range tc.Capture.FilterFileWrite {
		if len(filter) > 50 {
			return fmt.Errorf("The length of a path filter is limited to 50 characters: %s", filter)
		}
	}
	_, err := os.Stat(tc.BPFObjPath)
	if err == nil {
		return err
	}

	err = tc.Output.Validate()
	if err != nil {
		return err
	}
	return nil
}

type hookDescriptor struct {
	Filename   string   `json:"filename"`
	Offset     uint64   `json:"addr_off"`
	ClassName  string   `json:"class_name"`
	Method     string   `json:"method"`
	ArgsTypes  []string `json:"args_types"`
	ArgOff     int      `json:"arg_off"`
	MethodIdx  int      `json:"method_idx"`
	AddrCount  int      `json:"addr_count"`
	UprobeType string   `json:"uprobe_type"`
}

type apiHookConfig struct {
	ClassName  string `json:"class_name"`
	Method     string `json:"method"`
	ThisObject bool   `json:"thisObject"`
	HookType   string `json:"type"`
}

type libHookConfig struct {
	LibPath   string   `json:"lib_path"`
	SymName   string   `json:"sym_name"`
	Offset    string   `json:"offset"`
	ArgsTypes []string `json:"args_types"`
}

type syscallHookConfig struct {
	Name  string   `json:"name"`
}

type kprobeHookConfig struct {
	Name  string   `json:"name"`
}

type hooksConfigs struct {
	Api      []apiHookConfig     `json:"api"`
	Syscalls []syscallHookConfig `json:"syscalls"`
	Kprobes  []kprobeHookConfig  `json:"kprobes"`
	Uprobes  []libHookConfig     `json:"uprobes"`
}

type BpfroidConfig struct {
	HookConfigs hooksConfigs `json:"hookConfigs"`
	Trace       bool         `json:"trace"`
}

// Tracee traces system calls and system events using eBPF
type Tracee struct {
	config            TraceeConfig
	eventsToTrace     map[int32]bool
	bpfModule         *bpf.Module
	eventsPerfMap     *bpf.PerfBuffer
	fileWrPerfMap     *bpf.PerfBuffer
	eventsChannel     chan []byte
	fileWrChannel     chan []byte
	lostEvChannel     chan uint64
	lostWrChannel     chan uint64
	printer           eventPrinter
	stats             statsStore
	capturedFiles     map[string]int64
	writtenFiles      map[string]string
	mntNsFirstPid     map[uint32]uint32
	DecParamName      [2]map[argTag]external.ArgMeta
	EncParamName      [2]map[string]argTag
	pidsInMntns       bucketsCache //record the first n PIDs (host) in each mount namespace, for internal usage
	StackAddressesMap *bpf.BPFMap
	// Hooks data
	soBases     map[string]uint64
	oatBases    map[string]uint64
	oatdataOffs map[string]uint64
	uprobesDesc map[uint64]hookDescriptor
	apiHooks    []hookDescriptor
	noCodeHooks []hookDescriptor
}

type counter int32

func (c *counter) Increment(amount ...int) {
	sum := 1
	if len(amount) > 0 {
		sum = 0
		for _, a := range amount {
			sum = sum + a
		}
	}
	atomic.AddInt32((*int32)(c), int32(sum))
}

type statsStore struct {
	eventCounter  counter
	errorCounter  counter
	lostEvCounter counter
	lostWrCounter counter
}

// New creates a new Tracee instance based on a given valid TraceeConfig
func New(cfg TraceeConfig) (*Tracee, error) {
	var err error

	err = cfg.Validate()
	if err != nil {
		return nil, fmt.Errorf("validation error: %v", err)
	}

	setEssential := func(id int32) {
		event := EventsIDToEvent[id]
		event.EssentialEvent = true
		EventsIDToEvent[id] = event
	}
	if cfg.Capture.Exec {
		setEssential(SecurityBprmCheckEventID)
	}
	if cfg.Capture.FileWrite {
		setEssential(VfsWriteEventID)
		setEssential(VfsWritevEventID)
	}
	if cfg.SecurityAlerts || cfg.Capture.Mem {
		setEssential(MmapEventID)
		setEssential(MprotectEventID)
	}
	if cfg.Capture.Mem {
		setEssential(MemProtAlertEventID)
	}
	// create tracee
	t := &Tracee{
		config: cfg,
	}
	outf := os.Stdout
	if t.config.Output.OutPath != "" {
		dir := filepath.Dir(t.config.Output.OutPath)
		os.MkdirAll(dir, 0755)
		os.Remove(t.config.Output.OutPath)
		outf, err = os.Create(t.config.Output.OutPath)
		if err != nil {
			return nil, err
		}
	}
	errf := os.Stderr
	if t.config.Output.ErrPath != "" {
		dir := filepath.Dir(t.config.Output.ErrPath)
		os.MkdirAll(dir, 0755)
		os.Remove(t.config.Output.ErrPath)
		errf, err = os.Create(t.config.Output.ErrPath)
		if err != nil {
			return nil, err
		}
	}
	ContainerMode := (t.config.Filter.ContFilter.Enabled && t.config.Filter.ContFilter.Value) ||
		(t.config.Filter.NewContFilter.Enabled && t.config.Filter.NewContFilter.Value)
	printObj, err := newEventPrinter(t.config.Output.Format, ContainerMode, t.config.Output.EOT, outf, errf)
	if err != nil {
		return nil, err
	}
	t.printer = printObj
	t.eventsToTrace = make(map[int32]bool, len(t.config.Filter.EventsToTrace))
	for _, e := range t.config.Filter.EventsToTrace {
		// Map value is true iff events requested by the user
		t.eventsToTrace[e] = true
	}

	// Compile final list of events to trace including essential events
	for id, event := range EventsIDToEvent {
		// If an essential event was not requested by the user, set its map value to false
		if event.EssentialEvent && !t.eventsToTrace[id] {
			t.eventsToTrace[id] = false
		}
	}

	t.DecParamName[0] = make(map[argTag]external.ArgMeta)
	t.EncParamName[0] = make(map[string]argTag)
	t.DecParamName[1] = make(map[argTag]external.ArgMeta)
	t.EncParamName[1] = make(map[string]argTag)

	t.soBases = make(map[string]uint64)
	t.oatBases = make(map[string]uint64)
	t.oatdataOffs = make(map[string]uint64)
	t.uprobesDesc = make(map[uint64]hookDescriptor)

	err = t.initBPF(cfg.BPFObjPath)
	if err != nil {
		t.Close()
		return nil, err
	}

	t.writtenFiles = make(map[string]string)
	t.capturedFiles = make(map[string]int64)
	//set a default value for config.maxPidsCache
	if t.config.maxPidsCache == 0 {
		t.config.maxPidsCache = 5
	}
	t.pidsInMntns.Init(t.config.maxPidsCache)

	hostMntnsLink, err := os.Readlink("/proc/1/ns/mnt")
	if err == nil {
		hostMntnsString := strings.TrimSuffix(strings.TrimPrefix(hostMntnsLink, "mnt:["), "]")
		hostMntns, err := strconv.Atoi(hostMntnsString)
		if err == nil {
			t.pidsInMntns.AddBucketItem(uint32(hostMntns), 1)
		}
	}

	if err := os.MkdirAll(t.config.Capture.OutputPath, 0755); err != nil {
		return nil, fmt.Errorf("error creating output path: %v", err)
	}
	// Todo: tracee.pid should be in a known constant location. /var/run is probably a better choice
	err = ioutil.WriteFile(path.Join(t.config.Capture.OutputPath, "tracee.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0640)
	if err != nil {
		return nil, fmt.Errorf("error creating readiness file: %v", err)
	}

	// Get refernce to stack trace addresses map
	StackAddressesMap, err := t.bpfModule.GetMap("stack_addresses")
	if err != nil {
		return nil, fmt.Errorf("error getting acces to 'stack_addresses' eBPF Map %v", err)
	}
	t.StackAddressesMap = StackAddressesMap

	return t, nil
}

type bucketsCache struct {
	buckets     map[uint32][]uint32
	bucketLimit int
	Null        uint32
}

func (c *bucketsCache) Init(bucketLimit int) {
	c.bucketLimit = bucketLimit
	c.buckets = make(map[uint32][]uint32)
	c.Null = 0
}

func (c *bucketsCache) GetBucket(key uint32) []uint32 {
	return c.buckets[key]
}

func (c *bucketsCache) GetBucketItem(key uint32, index int) uint32 {
	b, exists := c.buckets[key]
	if !exists {
		return c.Null
	}
	if index >= len(b) {
		return c.Null
	}
	return b[index]
}

func (c *bucketsCache) AddBucketItem(key uint32, value uint32) {
	c.addBucketItem(key, value, false)
}

func (c *bucketsCache) ForceAddBucketItem(key uint32, value uint32) {
	c.addBucketItem(key, value, true)
}

func (c *bucketsCache) addBucketItem(key uint32, value uint32, force bool) {
	b, exists := c.buckets[key]
	if !exists {
		c.buckets[key] = make([]uint32, 0, c.bucketLimit)
		b = c.buckets[key]
	}
	if len(b) >= c.bucketLimit {
		if force {
			b[0] = value
		} else {
			return
		}
	} else {
		c.buckets[key] = append(b, value)
	}
}

// UnameRelease gets the version string of the current running kernel
func UnameRelease() string {
	var uname syscall.Utsname
	if err := syscall.Uname(&uname); err != nil {
		return ""
	}
	var buf [65]byte
	for i, b := range uname.Release {
		buf[i] = byte(b)
	}
	ver := string(buf[:])
	if i := strings.Index(ver, "\x00"); i != -1 {
		ver = ver[:i]
	}
	return ver
}

func supportRawTP() (bool, error) {
	ver := UnameRelease()
	if ver == "" {
		return false, fmt.Errorf("could not determine current release")
	}
	ver_split := strings.Split(ver, ".")
	if len(ver_split) < 2 {
		return false, fmt.Errorf("invalid version returned by uname")
	}
	major, err := strconv.Atoi(ver_split[0])
	if err != nil {
		return false, fmt.Errorf("invalid major number: %s", ver_split[0])
	}
	minor, err := strconv.Atoi(ver_split[1])
	if err != nil {
		return false, fmt.Errorf("invalid minor number: %s", ver_split[1])
	}
	if ((major == 4) && (minor >= 17)) || (major > 4) {
		return true, nil
	}
	return false, nil
}

type eventParam struct {
	encType argType
	encName argTag
}

func encParamType(Type string) argType {
	switch Type {
	case "int", "pid_t", "uid_t", "gid_t", "mqd_t", "clockid_t", "const clockid_t", "key_t", "key_serial_t", "timer_t":
		return intT
	case "unsigned int", "u32":
		return uintT
	case "long", "int64":
		return longT
	case "unsigned long", "u64":
		return ulongT
	case "off_t":
		return offT
	case "mode_t":
		return modeT
	case "dev_t":
		return devT
	case "size_t":
		return sizeT
	case "void*", "const void*":
		return pointerT
	case "char*", "const char*":
		return strT
	case "const char*const*", "const char**", "char**":
		return strArrT
	case "const struct sockaddr*", "struct sockaddr*":
		return sockAddrT
	default:
		//fmt.Printf("Unsupported parameter type: %s\n", Type)
		// Default to pointer (printed as hex) for unsupported types
		return pointerT
	}
}

func (t *Tracee) initEventsParams() map[int32][]eventParam {
	eventsParams := make(map[int32][]eventParam)
	var seenNames [2]map[string]bool
	var ParamNameCounter [2]argTag
	seenNames[0] = make(map[string]bool)
	ParamNameCounter[0] = argTag(1)
	seenNames[1] = make(map[string]bool)
	ParamNameCounter[1] = argTag(1)
	paramT := noneT
	for id, params := range EventsIDToParams {
		for _, param := range params {
			paramT = encParamType(param.Type)

			// As the encoded parameter name is u8, it can hold up to 256 different names
			// To keep on low communication overhead, we don't change this to u16
			// Instead, use an array of enc/dec maps, where the key is modulus of the event id
			// This can easilly be expanded in the future if required
			if !seenNames[id%2][param.Name] {
				seenNames[id%2][param.Name] = true
				t.EncParamName[id%2][param.Name] = ParamNameCounter[id%2]
				t.DecParamName[id%2][ParamNameCounter[id%2]] = param
				eventsParams[id] = append(eventsParams[id], eventParam{encType: paramT, encName: ParamNameCounter[id%2]})
				ParamNameCounter[id%2]++
			} else {
				eventsParams[id] = append(eventsParams[id], eventParam{encType: paramT, encName: t.EncParamName[id%2][param.Name]})
			}
		}
	}

	if len(seenNames[0]) > 255 || len(seenNames[1]) > 255 {
		panic("Too many argument names given")
	}

	return eventsParams
}

// find "zygote" in /proc/pid/cmdline to get zygote pid
// then iterate over its maps to get oat files base addresses
// use these values to build a map of oat->base
func (t *Tracee) initLibBases() error {
	fmt.Printf("Extracting oat files and offsets from zygote process...\n")
	d, err := os.Open("/proc")
	if err != nil {
		return err
	}
	defer d.Close()

	names, err := d.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("could not read %s: %s", d.Name(), err)
	}

	oatBase := make(map[string]uint64)
	var oatExec []string
	soBase := make(map[string]uint64)
	var soExec []string

	zygoteFound := false
	zygote64Found := false
	zygotePid := ""

	for _, dirname := range names {
		_, err := strconv.ParseInt(dirname, 10, 64)
		if err != nil {
			continue
		}

		cmdLinePath := "/proc/" + dirname + "/cmdline"
		f, err := os.Open(cmdLinePath)
		if err != nil {
			continue
		}
		defer f.Close()

		reader := io.LimitReader(f, 16)
		data, err := ioutil.ReadAll(reader)
		if err != nil {
			continue
		}

		if len(data) < 1 {
			continue
		}

		content := string(bytes.TrimRight(data, string("\x00")))
		if content == "zygote64" {
			zygote64Found = true
			fmt.Printf("Found zygote64 pid: %s\n", dirname) // print zygote64 pid
			zygotePid = dirname
		}
		if content == "zygote" {
			zygoteFound = true
			fmt.Printf("Found zygote pid: %s\n", dirname) // print zygote pid
			// Prefer zygote64 over zygote
			if !zygote64Found {
				zygotePid = dirname
			}
		}
	}

	if !zygoteFound && !zygote64Found {
		return fmt.Errorf("Failed to find zygote process")
	}

	mapsPath := "/proc/" + zygotePid + "/maps"
	mapsFile, err := os.Open(mapsPath)
	if err != nil {
		return err
	}
	defer mapsFile.Close()

	scanner := bufio.NewScanner(mapsFile)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasSuffix(line, ".oat") {
			splitLine := strings.Fields(line)
			startAddr, err := strconv.ParseUint(strings.Split(splitLine[0], "-")[0], 16, 64)
			if err != nil {
				return err
			}
			perms := splitLine[1]
			filename := splitLine[5]
			if base, ok := oatBase[filename]; ok {
				if base > startAddr {
					oatBase[filename] = startAddr
				}
			} else {
				oatBase[filename] = startAddr
			}
			// check if executable
			if strings.Contains(perms, "x") {
				oatExec = append(oatExec, filename)
			}
		}

		if strings.HasSuffix(line, ".so") {
			splitLine := strings.Fields(line)
			startAddrStr := strings.Split(splitLine[0], "-")[0]
			startAddr, err := strconv.ParseUint(startAddrStr, 16, 64)
			if err != nil {
				return err
			}
			perms := splitLine[1]
			filename := splitLine[5]
			if base, ok := soBase[filename]; ok {
				if base > startAddr {
					soBase[filename] = startAddr
				}
			} else {
				soBase[filename] = startAddr
			}
			// check if executable
			if strings.Contains(perms, "x") {
				soExec = append(soExec, filename)
			}
		}
	}

	for _, so := range soExec {
		t.soBases[so] = soBase[so]
	}

	for _, oat := range oatExec {
		t.oatBases[oat] = oatBase[oat]
		oatdataOff := uint64(0x1000)
		f, err := os.Open(oat)
		if err != nil {
			return fmt.Errorf("Failed to open oat %s elf: %v", oat, err)
		}
		elfFile, err := elf.NewFile(f)
		if err != nil {
			return fmt.Errorf("Failed to open oat %s elf: %v", oat, err)
		}
		symbols, err := elfFile.DynamicSymbols()
		if err != nil {
			return fmt.Errorf("Failed to get %s symbols: %v", oat, err)
		}
		for _, symbol := range symbols {
			if symbol.Name == "oatdata" {
				oatdataOff = symbol.Value
				break
			}
		}
		fmt.Printf("+ %s base: 0x%x, oatdata offset: 0x%x\n", oat, oatBase[oat], oatdataOff)
		t.oatdataOffs[oat] = oatdataOff
	}

	return nil
}

func (t *Tracee) prepareApiUprobe(className string, methodName string, cachedApiHooks *[]hookDescriptor) error {
	// search in cache first
	cacheFound := false
	for _, hook := range *cachedApiHooks {
		if className == hook.ClassName && methodName == hook.Method {
			t.apiHooks = append(t.apiHooks, hook)
			cacheFound = true
		}
	}

	if cacheFound {
		return nil
	}

	// locate and set requested uprobes
	found := false
	ignore := true
	var argsTypes []string
	methodIdx := 0
	argOff := -1
	var addrOff uint64
	var addrCount int
	for oat, _ := range t.oatBases {
		out, err := exec.Command("oatdump", "--no-disassemble",
			"--oat-file="+oat, "--class-filter="+className,
			"--method-filter="+methodName).Output()
		if err != nil {
			return fmt.Errorf("command execution failed: %s", err)
		}
		scanner := bufio.NewScanner(bytes.NewReader(out))
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "dex_method_idx") {
				ignore = true
				argOff = -1
				splitLine := strings.Split(line, methodName+"(")
				if len(splitLine) < 2 {
					// this can happen if methodName is a substring of current method
					continue
				}
				trimedLine := splitLine[1]
				typesStr := strings.Split(trimedLine, ")")[0]
				argsTypes = strings.Split(typesStr, ", ")
				splitLine = strings.Split(trimedLine, "idx=")
				if len(splitLine) < 2 {
					// This can only happen if oatdump format changed
					fmt.Printf("Failed parsing oatdump output...\n")
					continue
				}
				methodIdxStr := strings.Split(splitLine[1], ")")[0]
				idx, err := strconv.ParseInt(methodIdxStr, 10, 32)
				methodIdx = int(idx)
				if err != nil {
					return err
				}
				ignore = false
			}
			if strings.Contains(line, "code_offset:") {
				if ignore {
					continue
				}
				found = true
				splitLine := strings.Split(line, "0x")
				if len(splitLine) < 2 {
					// This can only happen if oatdump format changed
					fmt.Printf("Failed parsing oatdump output...\n")
					continue
				}
				addrOffStr := strings.TrimSpace(splitLine[1])
				addrOff, err = strconv.ParseUint(addrOffStr, 16, 64)
				if err != nil {
					return err
				}
				if addrOff != 0x0 {
					// method offsets are realative to oatdata blob
					addrOff += t.oatdataOffs[oat]
					// set lsb to zero - relevant for arm arch. working with thumb
					addrOff = (addrOff >> 1) << 1
				}
				addrCount = strings.Count(string(out), addrOffStr)
			}
			if strings.Contains(line, "ins:") {
				if ignore {
					continue
				}
				splitLine := strings.Split(line, "#")
				firstArgOffStr := strings.Split(splitLine[1], "]")[0]
				argOff, err = strconv.Atoi(firstArgOffStr)
				if err != nil {
					argOff = -1
				}
			}
			if strings.Contains(line, "CODE: (code_offset") {
				if ignore {
					continue
				}
				// "CODE:..." should appear once at the output
				hook := hookDescriptor{
					Filename:   oat,
					Offset:     addrOff,
					ClassName:  className,
					Method:     methodName,
					ArgsTypes:  argsTypes,
					ArgOff:     argOff,
					MethodIdx:  methodIdx,
					AddrCount:  addrCount,
					UprobeType: "oat",
				}

				t.apiHooks = append(t.apiHooks, hook)
				*cachedApiHooks = append(*cachedApiHooks, hook)
				// clear argsTypes (should be initialized from dex_method_idx above)
				argsTypes = nil
				methodIdx = 0
				addrOff = 0
				addrCount = 0
				ignore = true
			}
		}
	}
	if !found {
		fmt.Printf("- Failed to find API function: %s.%s\n", className, methodName)
	}

	return nil
}

func (t *Tracee) setUprobe(hook hookDescriptor) error {
	// as we are in android - all base addresses are known in advance
	// use this fact to calculate the exact address of each function in memory
	var addr uint64
	if hook.UprobeType == "oat" {
		addr = t.oatBases[hook.Filename] + hook.Offset
	} else { // .so file
		addr = t.soBases[hook.Filename] + hook.Offset
	}
	if _, ok := t.uprobesDesc[addr]; ok {
		// this may occur if the same address is used multiple times
		return fmt.Errorf("? Not setting uprobe: %s.%s(%s) \n    in file: %s, offset: 0x%x: Already attached!\n",
			hook.ClassName, hook.Method, strings.Join(hook.ArgsTypes, ", "), hook.Filename, hook.Offset)
	}

	//save uprobe metadata to be used in events
	t.uprobesDesc[addr] = hook

	// get uprobe arguments types and encode in a single number
	var argsTypes uint64
	// var argsOff uint32

	// if hook.UprobeType == "oat" {
	// 	if hook.ArgOff >= 0 {
	// 		fmt.Printf("%d\n", hook.ArgOff)
	// 		argsOff = uint32(hook.ArgOff)
	// 	} else {
	// 		fmt.Printf("Warning: Function %s.%s arguments offset on stack is unknown. Assuming offset 8\n", hook.ClassName, hook.Method)
	// 		argsOff = 8
	// 	}
	// }

	for idx, argType := range hook.ArgsTypes {
		if argType == "" || argType == " " {
			continue
		}
		if idx > 5 {
			fmt.Printf("Warning: Function %s.%s has too many parameters. Last parameters will be ignored\n", hook.ClassName, hook.Method)
			break
		}
		argsTypes = argsTypes | (uint64(encParamType(argType)) << (8 * idx))
	}
	// set uprobe argument types
	// use uprobe exact address for "types_map" so we can get the correct types in real time
	paramsTypesBPFMap, _ := t.bpfModule.GetMap("types_map")
	paramsTypesBPFMap.Update(addr, argsTypes)

	if hook.UprobeType == "oat" {
		// set uprobe arguments offset from stack frame pointer
		// apiArgsOffBPFMap, _ := t.bpfModule.GetMap("api_args_off_map")
		// apiArgsOffBPFMap.Update(addr, argsOff)

		upEvent := fmt.Sprintf("%s.%s.%s", hook.ClassName, hook.Method, strings.Join(hook.ArgsTypes, "_"))
		// for setting uprobes - we should keep the offset from .oat base address
		fmt.Printf("+ %s.%s(%s) \n  in file: %s, offset: 0x%x\n",
			hook.ClassName, hook.Method, strings.Join(hook.ArgsTypes, ", "), hook.Filename, hook.Offset)
		prog, err := t.bpfModule.GetProgram("api_uprobe_ent_generic")
		if err != nil {
			return fmt.Errorf("error loading program api_uprobe_ent_generic: %v", err)
		}
		_, err = prog.AttachUprobeLegacy(upEvent, hook.Filename, hook.Offset)
		if err != nil {
			return fmt.Errorf("error attaching uprobe %s: %v", upEvent, err)
		}
	} else { // .so file
		fmt.Printf("+ \"%s\" in file: %s, offset: 0x%x\n", hook.Method, hook.Filename, hook.Offset)
		prog, err := t.bpfModule.GetProgram("func_trace_ent_generic")
		if err != nil {
			return fmt.Errorf("error loading program func_trace_ent_generic: %v", err)
		}
		_, err = prog.AttachUprobeLegacy(hook.Method, hook.Filename, hook.Offset)
		if err != nil {
			return fmt.Errorf("error attaching uprobe %s: %v", hook.Method, err)
		}
		prog, err = t.bpfModule.GetProgram("func_trace_ret_generic")
		if err != nil {
			return fmt.Errorf("error loading program func_trace_ret_generic: %v", err)
		}
		_, err = prog.AttachUretprobeLegacy(hook.Method, hook.Filename, hook.Offset)
		if err != nil {
			return fmt.Errorf("error attaching uprobe %s: %v", hook.Method, err)
		}
	}
	return nil
}

func (t *Tracee) initHooks() error {
	err := t.initLibBases()
	if err != nil {
		return err
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	cachedAPIHooksPath := filepath.Join(filepath.Dir(exePath), "api_hooks.cache")
	var cachedApiHooks []hookDescriptor
	if _, err := os.Stat(cachedAPIHooksPath); err == nil {
		hooksFile, err := ioutil.ReadFile(cachedAPIHooksPath)
		if err != nil {
			return err
		}

		fmt.Printf("Using cached hooks from %s\n", cachedAPIHooksPath)
		json.Unmarshal(hooksFile, &cachedApiHooks)
	}

	// attaching lib uprobes
	if len(t.config.LibHookConfigs) > 0 {
		fmt.Printf("\nAttaching uprobes:\n")
	}
	for _, uprobe := range t.config.LibHookConfigs {
		libPath := uprobe.LibPath
		symName := uprobe.SymName
		argsTypes := uprobe.ArgsTypes
		// todo: validate args type by comparing to known supported type list
		offset := uprobe.Offset
		if _, ok := t.soBases[libPath]; !ok {
			fmt.Printf("Failed setting uprobe: lib %s does not exist in zygote memory map!\n", libPath)
			continue
		}

		var hook hookDescriptor
		hook.Filename = libPath
		if offset != "" {
			hook.Offset, err = strconv.ParseUint(offset[2:], 16, 64)
			if err != nil {
				fmt.Printf("Failed setting uprobe: Invalid offset: %s\n", offset)
				continue
			}
		} else {
			hook.Offset = 0
			f, err := os.Open(libPath)
			if err != nil {
				return fmt.Errorf("Failed to open %s elf: %v", libPath, err)
			}
			elfFile, err := elf.NewFile(f)
			if err != nil {
				return fmt.Errorf("Failed to open %s elf: %v", libPath, err)
			}
			symbols, err := elfFile.DynamicSymbols()
			if err != nil {
				return fmt.Errorf("Failed to get %s symbols: %v", libPath, err)
			}
			for _, symbol := range symbols {
				if symbol.Name == symName {
					hook.Offset = symbol.Value
					break
				}
			}
		}
		hook.ClassName = ""
		hook.Method = symName
		hook.ArgsTypes = argsTypes
		hook.MethodIdx = 0
		hook.AddrCount = 1
		hook.UprobeType = "so"
		err = t.setUprobe(hook)
		if err != nil {
			fmt.Printf("%v\n", err)
		}
	}

	if len(t.config.ApiHookConfigs) > 0 {
		fmt.Printf("\nAttaching Android API uprobes:\n")
	}
	// prepare api uprobes metadata (e.g. offset of method in file)
	// full parsing should run only once per new environment (oat files didn't change)
	if len(cachedApiHooks) == 0 {
		fmt.Printf("Preparing hooks data from oat files (once), this may take a while...\n")
	}
	for _, apiHookConfig := range t.config.ApiHookConfigs {
		className := apiHookConfig.ClassName
		methodName := apiHookConfig.Method
		err = t.prepareApiUprobe(className, methodName, &cachedApiHooks)
		if err != nil {
			fmt.Printf("Failed preparing API uprobe: %v\n", err)
		}
	}

	// attaching api uprobes
	for _, hook := range t.apiHooks {
		if hook.Offset == 0x0 {
			t.noCodeHooks = append(t.noCodeHooks, hook)
			continue
		}
		// In oatdump, an address of a function has 3 occurences (this may change!)
		if hook.AddrCount > 3 {
			fmt.Printf("Warning: too many occurances (%d) of addr %x - multi usage?\n", hook.AddrCount, hook.Offset)
		}

		err = t.setUprobe(hook)
		if err != nil {
			fmt.Printf("%v\n", err)
		}
	}

	// print failed because no compiled code exists
	if len(t.noCodeHooks) > 0 {
		fmt.Printf("\nFailed setting the following uprobes: No compiled code found!:\n")
	}
	for _, hook := range t.noCodeHooks {
		fmt.Printf("- %s.%s(%s) \n    in file: %s\n",
			hook.ClassName, hook.Method, strings.Join(hook.ArgsTypes, ", "), hook.Filename)
	}

	// save parsed hook for next execution - save in chrooted env.
	_, err = os.Stat(cachedAPIHooksPath)
	if len(t.apiHooks) > 0 && ((len(cachedApiHooks) < len(t.apiHooks)) || os.IsNotExist(err))  {
		fileData, _ := json.MarshalIndent(cachedApiHooks, "", " ")
		_ = ioutil.WriteFile(cachedAPIHooksPath, fileData, 0644)
	}

	return nil
}

func (t *Tracee) setUintFilter(filter *UintFilter, filterMapName string, configFilter bpfConfig, lessIdx uint32) error {
	if !filter.Enabled {
		return nil
	}

	equalityFilter, err := t.bpfModule.GetMap(filterMapName)
	if err != nil {
		return err
	}
	for i := 0; i < len(filter.Equal); i++ {
		if filter.Is32Bit {
			err = equalityFilter.Update(uint32(filter.Equal[i]), filterEqual)
		} else {
			err = equalityFilter.Update(filter.Equal[i], filterEqual)
		}
		if err != nil {
			return err
		}
	}
	for i := 0; i < len(filter.NotEqual); i++ {
		if filter.Is32Bit {
			err = equalityFilter.Update(uint32(filter.NotEqual[i]), filterNotEqual)
		} else {
			err = equalityFilter.Update(filter.NotEqual[i], filterNotEqual)
		}
		if err != nil {
			return err
		}
	}

	inequalityFilter, err := t.bpfModule.GetMap("inequality_filter")
	if err != nil {
		return err
	}

	err = inequalityFilter.Update(lessIdx, filter.Less)
	if err != nil {
		return err
	}
	err = inequalityFilter.Update(lessIdx+1, filter.Greater)
	if err != nil {
		return err
	}

	bpfConfigMap, err := t.bpfModule.GetMap("config_map")
	if err != nil {
		return err
	}
	if len(filter.Equal) > 0 && len(filter.NotEqual) == 0 && filter.Greater == GreaterNotSetUint && filter.Less == LessNotSetUint {
		bpfConfigMap.Update(uint32(configFilter), filterIn)
	} else {
		bpfConfigMap.Update(uint32(configFilter), filterOut)
	}

	return nil
}

func (t *Tracee) setStringFilter(filter *StringFilter, filterMapName string, configFilter bpfConfig) error {
	if !filter.Enabled {
		return nil
	}

	filterMap, err := t.bpfModule.GetMap(filterMapName)
	if err != nil {
		return err
	}
	for i := 0; i < len(filter.Equal); i++ {
		err = filterMap.Update([]byte(filter.Equal[i]), filterEqual)
		if err != nil {
			return err
		}
	}
	for i := 0; i < len(filter.NotEqual); i++ {
		err = filterMap.Update([]byte(filter.NotEqual[i]), filterNotEqual)
		if err != nil {
			return err
		}
	}

	bpfConfigMap, err := t.bpfModule.GetMap("config_map")
	if err != nil {
		return err
	}
	if len(filter.Equal) > 0 && len(filter.NotEqual) == 0 {
		bpfConfigMap.Update(uint32(configFilter), filterIn)
	} else {
		bpfConfigMap.Update(uint32(configFilter), filterOut)
	}

	return nil
}

func (t *Tracee) setBoolFilter(filter *BoolFilter, configFilter bpfConfig) error {
	if !filter.Enabled {
		return nil
	}

	bpfConfigMap, err := t.bpfModule.GetMap("config_map")
	if err != nil {
		return err
	}
	if filter.Value {
		bpfConfigMap.Update(uint32(configFilter), filterIn)
	} else {
		bpfConfigMap.Update(uint32(configFilter), filterOut)
	}

	return nil
}

func (t *Tracee) populateBPFMaps() error {
	chosenEventsMap, _ := t.bpfModule.GetMap("chosen_events_map")
	for e, chosen := range t.eventsToTrace {
		// Set chosen events map according to events chosen by the user
		if chosen {
			chosenEventsMap.Update(e, boolToUInt32(true))
		}

	}

	sys32to64BPFMap, _ := t.bpfModule.GetMap("sys_32_to_64_map")
	for _, event := range EventsIDToEvent {
		// Prepare 32bit to 64bit syscall number mapping
		sys32to64BPFMap.Update(event.ID32Bit, event.ID)
	}

	// Initialize config and pids maps
	bpfConfigMap, _ := t.bpfModule.GetMap("config_map")
	bpfConfigMap.Update(uint32(configDetectOrigSyscall), boolToUInt32(t.config.Output.DetectSyscall))
	bpfConfigMap.Update(uint32(configExecEnv), boolToUInt32(t.config.Output.ExecEnv))
	bpfConfigMap.Update(uint32(configStackAddresses), boolToUInt32(t.config.Output.StackAddresses))
	bpfConfigMap.Update(uint32(configCaptureFiles), boolToUInt32(t.config.Capture.FileWrite))
	bpfConfigMap.Update(uint32(configExtractDynCode), boolToUInt32(t.config.Capture.Mem))
	bpfConfigMap.Update(uint32(configTraceePid), uint32(os.Getpid()))
	bpfConfigMap.Update(uint32(configFollowFilter), boolToUInt32(t.config.Filter.Follow))

	// Initialize tail calls program array
	bpfProgArrayMap, _ := t.bpfModule.GetMap("prog_array")
	prog, err := t.bpfModule.GetProgram("trace_ret_vfs_write_tail")
	if err != nil {
		return fmt.Errorf("error getting BPF program trace_ret_vfs_write_tail: %v", err)
	}
	bpfProgArrayMap.Update(uint32(tailVfsWrite), uint32(prog.GetFd()))

	prog, err = t.bpfModule.GetProgram("trace_ret_vfs_writev_tail")
	if err != nil {
		return fmt.Errorf("error getting BPF program trace_ret_vfs_writev_tail: %v", err)
	}
	bpfProgArrayMap.Update(uint32(tailVfsWritev), uint32(prog.GetFd()))

	prog, err = t.bpfModule.GetProgram("send_bin")
	if err != nil {
		return fmt.Errorf("error getting BPF program send_bin: %v", err)
	}
	bpfProgArrayMap.Update(uint32(tailSendBin), uint32(prog.GetFd()))

	// Set filters given by the user to filter file write events
	fileFilterMap, _ := t.bpfModule.GetMap("file_filter")
	for i := 0; i < len(t.config.Capture.FilterFileWrite); i++ {
		fileFilterMap.Update(uint32(i), []byte(t.config.Capture.FilterFileWrite[i]))
	}

	err = t.setUintFilter(t.config.Filter.UIDFilter, "uid_filter", configUIDFilter, uidLess)
	if err != nil {
		return fmt.Errorf("error setting uid filter: %v", err)
	}

	err = t.setUintFilter(t.config.Filter.PIDFilter, "pid_filter", configPidFilter, pidLess)
	if err != nil {
		return fmt.Errorf("error setting pid filter: %v", err)
	}

	err = t.setBoolFilter(t.config.Filter.NewPidFilter, configNewPidFilter)
	if err != nil {
		return fmt.Errorf("error setting pid=new filter: %v", err)
	}

	err = t.setUintFilter(t.config.Filter.MntNSFilter, "mnt_ns_filter", configMntNsFilter, mntNsLess)
	if err != nil {
		return fmt.Errorf("error setting mntns filter: %v", err)
	}

	err = t.setUintFilter(t.config.Filter.PidNSFilter, "pid_ns_filter", configPidNsFilter, pidNsLess)
	if err != nil {
		return fmt.Errorf("error setting pidns filter: %v", err)
	}

	err = t.setStringFilter(t.config.Filter.UTSFilter, "uts_ns_filter", configUTSNsFilter)
	if err != nil {
		return fmt.Errorf("error setting uts_ns filter: %v", err)
	}

	err = t.setStringFilter(t.config.Filter.CommFilter, "comm_filter", configCommFilter)
	if err != nil {
		return fmt.Errorf("error setting comm filter: %v", err)
	}

	err = t.setBoolFilter(t.config.Filter.ContFilter, configContFilter)
	if err != nil {
		return fmt.Errorf("error setting cont filter: %v", err)
	}

	err = t.setBoolFilter(t.config.Filter.NewContFilter, configNewContFilter)
	if err != nil {
		return fmt.Errorf("error setting container=new filter: %v", err)
	}

	stringStoreMap, _ := t.bpfModule.GetMap("string_store")
	stringStoreMap.Update(uint32(0), []byte("/dev/null"))

	eventsParams := t.initEventsParams()

	// After initializing event params, we can also initialize argument filters argTags
	for eventID, eventFilters := range t.config.Filter.ArgFilter.Filters {
		for argName, filter := range eventFilters {
			argTag, ok := t.EncParamName[eventID%2][argName]
			if !ok {
				return fmt.Errorf("event argument %s for event %d was not initialized correctly", argName, eventID)
			}
			filter.argTag = argTag
			eventFilters[argName] = filter
		}
	}

	sysEnterTailsBPFMap, _ := t.bpfModule.GetMap("sys_enter_tails")
	//sysExitTailsBPFMap := t.bpfModule.GetMap("sys_exit_tails")
	paramsTypesBPFMap, _ := t.bpfModule.GetMap("params_types_map")
	paramsNamesBPFMap, _ := t.bpfModule.GetMap("params_names_map")
	for e := range t.eventsToTrace {
		params := eventsParams[e]
		var paramsTypes uint64
		var paramsNames uint64
		for n, param := range params {
			paramsTypes = paramsTypes | (uint64(param.encType) << (8 * n))
			paramsNames = paramsNames | (uint64(param.encName) << (8 * n))
		}
		paramsTypesBPFMap.Update(e, paramsTypes)
		paramsNamesBPFMap.Update(e, paramsNames)

		if e == ExecveEventID || e == ExecveatEventID {
			event, ok := EventsIDToEvent[e]
			if !ok {
				continue
			}

			probFnName := fmt.Sprintf("syscall__%s", event.Name)

			// execve functions require tail call on syscall enter as they perform extra work
			prog, err := t.bpfModule.GetProgram(probFnName)
			if err != nil {
				return fmt.Errorf("error loading BPF program %s: %v", probFnName, err)
			}
			sysEnterTailsBPFMap.Update(e, int32(prog.GetFd()))
		}
	}

	return nil
}

func (t *Tracee) initBPF(bpfObjectPath string) error {
	var err error

	t.bpfModule, err = bpf.NewModuleFromFile(bpfObjectPath)
	if err != nil {
		return err
	}

	supportRawTracepoints, err := supportRawTP()
	if err != nil {
		return fmt.Errorf("Failed to find kernel version: %v", err)
	}

	// BPFLoadObject() automatically loads ALL BPF programs according to their section type, unless set otherwise
	// For every BPF program, we need to make sure that:
	// 1. We disable autoload if the program is not required by any event and is not essential
	// 2. The correct BPF program type is set
	for _, event := range EventsIDToEvent {
		for _, probe := range event.Probes {
			prog, _ := t.bpfModule.GetProgram(probe.fn)
			if prog == nil && probe.attach == sysCall {
				prog, _ = t.bpfModule.GetProgram(fmt.Sprintf("syscall__%s", probe.fn))
			}
			if prog == nil {
				continue
			}
			if _, ok := t.eventsToTrace[event.ID]; !ok {
				// This event is not being traced - set its respective program(s) "autoload" to false
				err = prog.SetAutoload(false)
				if err != nil {
					return err
				}
				continue
			}
			// As kernels < 4.17 don't support raw tracepoints, set these program types to "regular" tracepoint
			if !supportRawTracepoints && (prog.GetType() == bpf.BPFProgTypeRawTracepoint) {
				err = prog.SetTracepoint()
				if err != nil {
					return err
				}
			}
		}
	}

	err = t.bpfModule.BPFLoadObject()
	if err != nil {
		return err
	}

	err = t.populateBPFMaps()
	if err != nil {
		return err
	}

	err = t.initHooks()
	if err != nil {
		return err
	}

	for e, _ := range t.eventsToTrace {
		event, ok := EventsIDToEvent[e]
		if !ok {
			continue
		}
		for _, probe := range event.Probes {
			if probe.attach == sysCall {
				// Already handled by raw_syscalls tracepoints
				continue
			}
			prog, err := t.bpfModule.GetProgram(probe.fn)
			if err != nil {
				return fmt.Errorf("error getting program %s: %v", probe.fn, err)
			}
			if probe.attach == rawTracepoint && !supportRawTracepoints {
				// We fallback to regular tracepoint in case kernel doesn't support raw tracepoints (< 4.17)
				probe.attach = tracepoint
			}
			switch probe.attach {
			case kprobe:
				// todo: after updating minimal kernel version to 4.18, use without legacy
				_, err = prog.AttachKprobeLegacy(probe.event)
			case kretprobe:
				// todo: after updating minimal kernel version to 4.18, use without legacy
				_, err = prog.AttachKretprobeLegacy(probe.event)
			case tracepoint:
				_, err = prog.AttachTracepoint(probe.event)
			case rawTracepoint:
				tpEvent := strings.Split(probe.event, ":")[1]
				_, err = prog.AttachRawTracepoint(tpEvent)
			}
			if err != nil {
				return fmt.Errorf("error attaching event %s: %v", probe.event, err)
			}
		}
	}

	// Initialize perf buffers
	t.eventsChannel = make(chan []byte, 1000)
	t.lostEvChannel = make(chan uint64)
	t.eventsPerfMap, err = t.bpfModule.InitPerfBuf("events", t.eventsChannel, t.lostEvChannel, t.config.PerfBufferSize)
	if err != nil {
		return fmt.Errorf("error initializing events perf map: %v", err)
	}

	t.fileWrChannel = make(chan []byte, 1000)
	t.lostWrChannel = make(chan uint64)
	t.fileWrPerfMap, err = t.bpfModule.InitPerfBuf("file_writes", t.fileWrChannel, t.lostWrChannel, t.config.BlobPerfBufferSize)
	if err != nil {
		return fmt.Errorf("error initializing file_writes perf map: %v", err)
	}

	return nil
}

// Run starts the trace. it will run until interrupted
func (t *Tracee) Run() error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	done := make(chan struct{})
	t.printer.Preamble()
	t.eventsPerfMap.Start()
	t.fileWrPerfMap.Start()
	go t.processLostEvents()
	go t.runEventPipeline(done)
	go t.processFileWrites()
	<-sig
	t.eventsPerfMap.Stop()
	t.fileWrPerfMap.Stop()
	t.printer.Epilogue(t.stats)

	// record index of written files
	if t.config.Capture.FileWrite {
		destinationFilePath := filepath.Join(t.config.Capture.OutputPath, "written_files")
		f, err := os.OpenFile(destinationFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("error logging written files")
		}
		defer f.Close()
		for fileName, filePath := range t.writtenFiles {
			writeFiltered := false
			for _, filterPrefix := range t.config.Capture.FilterFileWrite {
				if !strings.HasPrefix(filePath, filterPrefix) {
					writeFiltered = true
					break
				}
			}
			if writeFiltered {
				// Don't write mapping of files that were not actually captured
				continue
			}
			if _, err := f.WriteString(fmt.Sprintf("%s %s\n", fileName, filePath)); err != nil {
				return fmt.Errorf("error logging written files")
			}
		}
	}

	// Signal pipeline that Tracee exits by closing the done channel
	close(done)
	t.Close()
	return nil
}

// Close cleans up created resources
func (t *Tracee) Close() {
	if t.bpfModule != nil {
		t.bpfModule.Close()
	}
	t.printer.Close()
}

func boolToUInt32(b bool) uint32 {
	if b {
		return uint32(1)
	}
	return uint32(0)
}

// CopyFileByPath copies a file from src to dst
func CopyFileByPath(src, dst string) error {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !sourceFileStat.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()
	_, err = io.Copy(destination, source)
	if err != nil {
		return err
	}
	return nil
}

func (t *Tracee) handleError(err error) {
	t.stats.errorCounter.Increment()
	t.printer.Error(err)
}

// shouldProcessEvent decides whether or not to drop an event before further processing it
func (t *Tracee) shouldProcessEvent(e RawEvent) bool {
	if t.config.Filter.RetFilter.Enabled {
		if filter, ok := t.config.Filter.RetFilter.Filters[e.Ctx.EventID]; ok {
			retVal := e.Ctx.Retval
			match := false
			for _, f := range filter.Equal {
				if retVal == f {
					match = true
					break
				}
			}
			if !match && len(filter.Equal) > 0 {
				return false
			}
			for _, f := range filter.NotEqual {
				if retVal == f {
					return false
				}
			}
			if (filter.Greater != GreaterNotSetInt) && retVal <= filter.Greater {
				return false
			}
			if (filter.Less != LessNotSetInt) && retVal >= filter.Less {
				return false
			}
		}
	}

	if t.config.Filter.ArgFilter.Enabled {
		for _, filter := range t.config.Filter.ArgFilter.Filters[e.Ctx.EventID] {
			argVal, ok := e.RawArgs[filter.argTag]
			if !ok {
				continue
			}
			// TODO: use type assertion instead of string convertion
			argValStr := fmt.Sprint(argVal)
			match := false
			for _, f := range filter.Equal {
				if argValStr == f || (f[len(f)-1] == '*' && strings.HasPrefix(argValStr, f[0:len(f)-1])) {
					match = true
					break
				}
			}
			if !match && len(filter.Equal) > 0 {
				return false
			}
			for _, f := range filter.NotEqual {
				if argValStr == f || (f[len(f)-1] == '*' && strings.HasPrefix(argValStr, f[0:len(f)-1])) {
					return false
				}
			}
		}
	}

	return true
}

func (t *Tracee) processEvent(ctx *context, args map[argTag]interface{}) error {
	switch ctx.EventID {

	//capture written files
	case VfsWriteEventID, VfsWritevEventID:
		if t.config.Capture.FileWrite {
			filePath, ok := args[t.EncParamName[ctx.EventID%2]["pathname"]].(string)
			if !ok {
				return fmt.Errorf("error parsing vfs_write args")
			}
			// path should be absolute, except for e.g memfd_create files
			if filePath == "" || filePath[0] != '/' {
				return nil
			}
			dev, ok := args[t.EncParamName[ctx.EventID%2]["dev"]].(uint32)
			if !ok {
				return fmt.Errorf("error parsing vfs_write args")
			}
			inode, ok := args[t.EncParamName[ctx.EventID%2]["inode"]].(uint64)
			if !ok {
				return fmt.Errorf("error parsing vfs_write args")
			}

			// stop processing if write was already indexed
			fileName := fmt.Sprintf("%d/write.dev-%d.inode-%d", ctx.MntID, dev, inode)
			indexName, ok := t.writtenFiles[fileName]
			if ok && indexName == filePath {
				return nil
			}

			// index written file by original filepath
			t.writtenFiles[fileName] = filePath
		}

	case SecurityBprmCheckEventID:

		//cache this pid by it's mnt ns
		if ctx.Pid == 1 {
			t.pidsInMntns.ForceAddBucketItem(ctx.MntID, ctx.HostPid)
		} else {
			t.pidsInMntns.AddBucketItem(ctx.MntID, ctx.HostPid)
		}

		//capture executed files
		if t.config.Capture.Exec {
			filePath, ok := args[t.EncParamName[ctx.EventID%2]["pathname"]].(string)
			if !ok {
				return fmt.Errorf("error parsing security_bprm_check args")
			}
			// path should be absolute, except for e.g memfd_create files
			if filePath == "" || filePath[0] != '/' {
				return nil
			}

			destinationDirPath := filepath.Join(t.config.Capture.OutputPath, strconv.Itoa(int(ctx.MntID)))
			if err := os.MkdirAll(destinationDirPath, 0755); err != nil {
				return err
			}
			destinationFilePath := filepath.Join(destinationDirPath, fmt.Sprintf("exec.%d.%s", ctx.Ts, filepath.Base(filePath)))

			var err error
			// try to access the root fs via another process in the same mount namespace (since the current process might have already died)
			pids := t.pidsInMntns.GetBucket(ctx.MntID)
			for _, pid := range pids { // will break on success
				err = nil
				sourceFilePath := fmt.Sprintf("/proc/%s/root%s", strconv.Itoa(int(pid)), filePath)
				var sourceFileStat os.FileInfo
				sourceFileStat, err = os.Stat(sourceFilePath)
				if err != nil {
					//TODO: remove dead pid from cache
					continue
				}
				//don't capture same file twice unless it was modified
				sourceFileCtime := sourceFileStat.Sys().(*syscall.Stat_t).Ctim.Nano()
				capturedFileID := fmt.Sprintf("%d:%s", ctx.MntID, sourceFilePath)
				lastCtime, ok := t.capturedFiles[capturedFileID]
				if ok && lastCtime == sourceFileCtime {
					return nil
				}
				//capture
				err = CopyFileByPath(sourceFilePath, destinationFilePath)
				if err != nil {
					return err
				}
				//mark this file as captured
				t.capturedFiles[capturedFileID] = sourceFileCtime
				break
			}
			return err
		}
	}

	return nil
}

// shouldPrintEvent decides whether or not the given event id should be printed to the output
func (t *Tracee) shouldPrintEvent(e RawEvent) bool {
	// Only print events requested by the user
	if !t.eventsToTrace[e.Ctx.EventID] {
		return false
	}
	return true
}

func (t *Tracee) prepareArgsForPrint(ctx *context, args map[argTag]interface{}) error {
	for key, arg := range args {
		if ptr, isUintptr := arg.(uintptr); isUintptr {
			args[key] = fmt.Sprintf("0x%X", ptr)
		}
	}
	switch ctx.EventID {
	case SysEnterEventID, SysExitEventID, CapCapableEventID:
		//show syscall name instead of id
		if id, isInt32 := args[t.EncParamName[ctx.EventID%2]["syscall"]].(int32); isInt32 {
			if event, isKnown := EventsIDToEvent[id]; isKnown {
				if event.Probes[0].attach == sysCall {
					args[t.EncParamName[ctx.EventID%2]["syscall"]] = event.Probes[0].event
				}
			}
		}
		if ctx.EventID == CapCapableEventID {
			if cap, isInt32 := args[t.EncParamName[ctx.EventID%2]["cap"]].(int32); isInt32 {
				args[t.EncParamName[ctx.EventID%2]["cap"]] = PrintCapability(cap)
			}
		}
	case MmapEventID, MprotectEventID, PkeyMprotectEventID:
		if prot, isInt32 := args[t.EncParamName[ctx.EventID%2]["prot"]].(int32); isInt32 {
			args[t.EncParamName[ctx.EventID%2]["prot"]] = PrintMemProt(uint32(prot))
		}
	case PtraceEventID:
		if req, isInt64 := args[t.EncParamName[ctx.EventID%2]["request"]].(int64); isInt64 {
			args[t.EncParamName[ctx.EventID%2]["request"]] = PrintPtraceRequest(req)
		}
	case PrctlEventID:
		if opt, isInt32 := args[t.EncParamName[ctx.EventID%2]["option"]].(int32); isInt32 {
			args[t.EncParamName[ctx.EventID%2]["option"]] = PrintPrctlOption(opt)
		}
	case SocketEventID:
		if dom, isInt32 := args[t.EncParamName[ctx.EventID%2]["domain"]].(int32); isInt32 {
			args[t.EncParamName[ctx.EventID%2]["domain"]] = PrintSocketDomain(uint32(dom))
		}
		if typ, isInt32 := args[t.EncParamName[ctx.EventID%2]["type"]].(int32); isInt32 {
			args[t.EncParamName[ctx.EventID%2]["type"]] = PrintSocketType(uint32(typ))
		}
	case ConnectEventID, AcceptEventID, Accept4EventID, BindEventID, GetsocknameEventID:
		if sockAddr, isStrMap := args[t.EncParamName[ctx.EventID%2]["addr"]].(map[string]string); isStrMap {
			var s string
			for key, val := range sockAddr {
				s += fmt.Sprintf("'%s': '%s',", key, val)
			}
			s = strings.TrimSuffix(s, ",")
			s = fmt.Sprintf("{%s}", s)
			args[t.EncParamName[ctx.EventID%2]["addr"]] = s
		}
	case AccessEventID, FaccessatEventID:
		if mode, isInt32 := args[t.EncParamName[ctx.EventID%2]["mode"]].(int32); isInt32 {
			args[t.EncParamName[ctx.EventID%2]["mode"]] = PrintAccessMode(uint32(mode))
		}
	case ExecveatEventID:
		if flags, isInt32 := args[t.EncParamName[ctx.EventID%2]["flags"]].(int32); isInt32 {
			args[t.EncParamName[ctx.EventID%2]["flags"]] = PrintExecFlags(uint32(flags))
		}
	case OpenEventID, OpenatEventID, SecurityFileOpenEventID:
		if flags, isInt32 := args[t.EncParamName[ctx.EventID%2]["flags"]].(int32); isInt32 {
			args[t.EncParamName[ctx.EventID%2]["flags"]] = PrintOpenFlags(uint32(flags))
		}
	case MknodEventID, MknodatEventID, ChmodEventID, FchmodEventID, FchmodatEventID:
		if mode, isUint32 := args[t.EncParamName[ctx.EventID%2]["mode"]].(uint32); isUint32 {
			args[t.EncParamName[ctx.EventID%2]["mode"]] = PrintInodeMode(mode)
		}
	case MemProtAlertEventID:
		if alert, isAlert := args[t.EncParamName[ctx.EventID%2]["alert"]].(alert); isAlert {
			args[t.EncParamName[ctx.EventID%2]["alert"]] = PrintAlert(alert)
		}
	case WriteAlertEventID:
		if magic, isUint32 := args[t.EncParamName[ctx.EventID%2]["magic"]].(uint32); isUint32 {
			if magic == 0x464c457f {
				args[t.EncParamName[ctx.EventID%2]["magic"]] = "Elf"
			}
			if magic == 0x04034b50 {
				args[t.EncParamName[ctx.EventID%2]["magic"]] = "Archive(zip/jar/apk)"
			}
			if magic == 0x0a786564 {
				args[t.EncParamName[ctx.EventID%2]["magic"]] = "Dex"
			}
		}
	case CloneEventID:
		if flags, isUint64 := args[t.EncParamName[ctx.EventID%2]["flags"]].(uint64); isUint64 {
			args[t.EncParamName[ctx.EventID%2]["flags"]] = PrintCloneFlags(flags)
		}
	case SendtoEventID, RecvfromEventID:
		addrTag := t.EncParamName[ctx.EventID%2]["dest_addr"]
		if ctx.EventID == RecvfromEventID {
			addrTag = t.EncParamName[ctx.EventID%2]["src_addr"]
		}
		if sockAddr, isStrMap := args[addrTag].(map[string]string); isStrMap {
			var s string
			for key, val := range sockAddr {
				s += fmt.Sprintf("'%s': '%s',", key, val)
			}
			s = strings.TrimSuffix(s, ",")
			s = fmt.Sprintf("{%s}", s)
			args[addrTag] = s
		}
	case BpfEventID:
		if cmd, isInt32 := args[t.EncParamName[ctx.EventID%2]["cmd"]].(int32); isInt32 {
			args[t.EncParamName[ctx.EventID%2]["cmd"]] = PrintBPFCmd(cmd)
		}
	case GenericUprobeEventID:
		// uprobe_addr sent as first argument
		uprobeOffTag := argTag(0)
		if addr, isUint64 := args[uprobeOffTag].(uint64); isUint64 {
			if _, ok := t.uprobesDesc[addr]; ok {
				args[uprobeOffTag] = t.uprobesDesc[addr].Filename + ":" + t.uprobesDesc[addr].Method
			} else {
				val := args[uprobeOffTag]
				args[uprobeOffTag] = fmt.Sprintf("0x%X", val)
			}
		}
	case GenericApiUprobeEventID:
		// uprobe_addr sent as first argument
		uprobeOffTag := argTag(0)
		if addr, isUint64 := args[uprobeOffTag].(uint64); isUint64 {
			if _, ok := t.uprobesDesc[addr]; ok {
				args[uprobeOffTag] = t.uprobesDesc[addr].ClassName + "." + t.uprobesDesc[addr].Method +  "(" + strings.Join(t.uprobesDesc[addr].ArgsTypes, ", ") + ")"
			} else {
				val := args[uprobeOffTag]
				args[uprobeOffTag] = fmt.Sprintf("0x%X", val)
			}
		}
	}

	return nil
}

// context struct contains common metadata that is collected for all types of events
// it is used to unmarshal binary data and therefore should match (bit by bit) to the `context_t` struct in the ebpf code.
// NOTE: Integers want to be aligned in memory, so if changing the format of this struct
// keep the 1-byte 'Argnum' as the final parameter before the padding (if padding is needed).
type context struct {
	Ts       uint64
	Pid      uint32
	Tid      uint32
	Ppid     uint32
	HostPid  uint32
	HostTid  uint32
	HostPpid uint32
	Uid      uint32
	MntID    uint32
	PidID    uint32
	Comm     [16]byte
	UtsName  [16]byte
	EventID  int32
	Retval   int64
	StackID  uint32
	Argnum   uint8
	_        [3]byte //padding
}

func (t *Tracee) processLostEvents() {
	for {
		lost := <-t.lostEvChannel
		t.stats.lostEvCounter.Increment(int(lost))
	}
}

func (t *Tracee) processFileWrites() {
	type chunkMeta struct {
		BinType  binType
		MntID    uint32
		Metadata [20]byte
		Size     int32
		Off      uint64
	}

	type vfsWriteMeta struct {
		DevID uint32
		Inode uint64
		Mode  uint32
		Pid   uint32
	}

	type mprotectWriteMeta struct {
		Ts uint64
	}

	const (
		S_IFMT uint32 = 0170000 // bit mask for the file type bit field

		S_IFSOCK uint32 = 0140000 // socket
		S_IFLNK  uint32 = 0120000 // symbolic link
		S_IFREG  uint32 = 0100000 // regular file
		S_IFBLK  uint32 = 0060000 // block device
		S_IFDIR  uint32 = 0040000 // directory
		S_IFCHR  uint32 = 0020000 // character device
		S_IFIFO  uint32 = 0010000 // FIFO
	)

	for {
		select {
		case dataRaw := <-t.fileWrChannel:
			dataBuff := bytes.NewBuffer(dataRaw)
			var meta chunkMeta
			appendFile := false
			err := binary.Read(dataBuff, binary.LittleEndian, &meta)
			if err != nil {
				t.handleError(err)
				continue
			}

			if meta.Size <= 0 {
				t.handleError(fmt.Errorf("error in file writer: invalid chunk size: %d", meta.Size))
				continue
			}
			if dataBuff.Len() < int(meta.Size) {
				t.handleError(fmt.Errorf("error in file writer: chunk too large: %d", meta.Size))
				continue
			}

			pathname := path.Join(t.config.Capture.OutputPath, strconv.Itoa(int(meta.MntID)))
			if err := os.MkdirAll(pathname, 0755); err != nil {
				t.handleError(err)
				continue
			}
			filename := ""
			metaBuff := bytes.NewBuffer(meta.Metadata[:])
			if meta.BinType == sendVfsWrite {
				var vfsMeta vfsWriteMeta
				err = binary.Read(metaBuff, binary.LittleEndian, &vfsMeta)
				if err != nil {
					t.handleError(err)
					continue
				}
				if vfsMeta.Mode&S_IFSOCK == S_IFSOCK || vfsMeta.Mode&S_IFCHR == S_IFCHR || vfsMeta.Mode&S_IFIFO == S_IFIFO {
					appendFile = true
				}
				if vfsMeta.Pid == 0 {
					filename = fmt.Sprintf("write.dev-%d.inode-%d", vfsMeta.DevID, vfsMeta.Inode)
				} else {
					filename = fmt.Sprintf("write.dev-%d.inode-%d.pid-%d", vfsMeta.DevID, vfsMeta.Inode, vfsMeta.Pid)
				}
			} else if meta.BinType == sendMprotect {
				var mprotectMeta mprotectWriteMeta
				err = binary.Read(metaBuff, binary.LittleEndian, &mprotectMeta)
				if err != nil {
					t.handleError(err)
					continue
				}
				// note: size of buffer will determine maximum extracted file size! (as writes from kernel are immediate)
				filename = fmt.Sprintf("bin.%d", mprotectMeta.Ts)
			} else {
				t.handleError(fmt.Errorf("error in file writer: unknown binary type: %d", meta.BinType))
				continue
			}

			fullname := path.Join(pathname, filename)

			f, err := os.OpenFile(fullname, os.O_CREATE|os.O_WRONLY, 0640)
			if err != nil {
				t.handleError(err)
				continue
			}
			if appendFile {
				if _, err := f.Seek(0, os.SEEK_END); err != nil {
					f.Close()
					t.handleError(err)
					continue
				}
			} else {
				if _, err := f.Seek(int64(meta.Off), os.SEEK_SET); err != nil {
					f.Close()
					t.handleError(err)
					continue
				}
			}

			dataBytes, err := readByteSliceFromBuff(dataBuff, int(meta.Size))
			if err != nil {
				f.Close()
				t.handleError(err)
				continue
			}
			if _, err := f.Write(dataBytes); err != nil {
				f.Close()
				t.handleError(err)
				continue
			}
			if err := f.Close(); err != nil {
				t.handleError(err)
				continue
			}
		case lost := <-t.lostWrChannel:
			t.stats.lostWrCounter.Increment(int(lost))
		}
	}
}

func readStringFromBuff(buff io.Reader) (string, error) {
	var err error
	var size uint32
	err = binary.Read(buff, binary.LittleEndian, &size)
	if err != nil {
		return "", fmt.Errorf("error reading string size: %v", err)
	}
	if size > 4096 {
		return "", fmt.Errorf("string size too big: %d", size)
	}
	res, err := readByteSliceFromBuff(buff, int(size-1)) //last byte is string terminating null
	defer func() {
		var dummy int8
		binary.Read(buff, binary.LittleEndian, &dummy) //discard last byte which is string terminating null
	}()
	if err != nil {
		return "", fmt.Errorf("error reading string arg: %v", err)
	}
	return string(res), nil
}

// readStringVarFromBuff reads a null-terminated string from `buff`
// max length can be passed as `max` to optimize memory allocation, otherwise pass 0
func readStringVarFromBuff(buff io.Reader, max int) (string, error) {
	var err error
	var char int8
	res := make([]byte, max)
	err = binary.Read(buff, binary.LittleEndian, &char)
	if err != nil {
		return "", fmt.Errorf("error reading null terminated string: %v", err)
	}
	for count := 1; char != 0 && count < max; count++ {
		res = append(res, byte(char))
		err = binary.Read(buff, binary.LittleEndian, &char)
		if err != nil {
			return "", fmt.Errorf("error reading null terminated string: %v", err)
		}
	}
	res = bytes.TrimLeft(res[:], "\000")
	return string(res), nil
}

func readByteSliceFromBuff(buff io.Reader, len int) ([]byte, error) {
	var err error
	res := make([]byte, len)
	err = binary.Read(buff, binary.LittleEndian, &res)
	if err != nil {
		return nil, fmt.Errorf("error reading byte array: %v", err)
	}
	return res, nil
}

func readSockaddrFromBuff(buff io.Reader) (map[string]string, error) {
	res := make(map[string]string, 3)
	var family int16
	err := binary.Read(buff, binary.LittleEndian, &family)
	if err != nil {
		return nil, err
	}
	res["sa_family"] = PrintSocketDomain(uint32(family))
	switch family {
	case 1: // AF_UNIX
		/*
			http://man7.org/linux/man-pages/man7/unix.7.html
			struct sockaddr_un {
					sa_family_t sun_family;     // AF_UNIX
					char        sun_path[108];  // Pathname
			};
		*/
		var sunPathBuf [108]byte
		err := binary.Read(buff, binary.LittleEndian, &sunPathBuf)
		if err != nil {
			return nil, fmt.Errorf("error parsing sockaddr_un: %v", err)
		}
		trimmedPath := bytes.TrimLeft(sunPathBuf[:], "\000")
		sunPath := ""
		if len(trimmedPath) != 0 {
			sunPath, err = readStringVarFromBuff(bytes.NewBuffer(trimmedPath), 108)
		}
		if err != nil {
			return nil, fmt.Errorf("error parsing sockaddr_un: %v", err)
		}
		res["sun_path"] = sunPath
	case 2: // AF_INET
		/*
			http://man7.org/linux/man-pages/man7/ip.7.html
			struct sockaddr_in {
				sa_family_t    sin_family; // address family: AF_INET
				in_port_t      sin_port;   // port in network byte order
				struct in_addr sin_addr;   // internet address
				// byte        padding[8]; //https://elixir.bootlin.com/linux/v4.20.17/source/include/uapi/linux/in.h#L232
			};
			struct in_addr {
				uint32_t       s_addr;     // address in network byte order
			};
		*/
		var port uint16
		err = binary.Read(buff, binary.BigEndian, &port)
		if err != nil {
			return nil, fmt.Errorf("error parsing sockaddr_in: %v", err)
		}
		res["sin_port"] = strconv.Itoa(int(port))
		var addr uint32
		err = binary.Read(buff, binary.BigEndian, &addr)
		if err != nil {
			return nil, fmt.Errorf("error parsing sockaddr_in: %v", err)
		}
		res["sin_addr"] = PrintUint32IP(addr)
		_, err := readByteSliceFromBuff(buff, 8)
		if err != nil {
			return nil, fmt.Errorf("error parsing sockaddr_in: %v", err)
		}
	case 10: // AF_INET6
		/*
			struct sockaddr_in6 {
				sa_family_t     sin6_family;   // AF_INET6
				in_port_t       sin6_port;     // port number
				uint32_t        sin6_flowinfo; // IPv6 flow information
				struct in6_addr sin6_addr;     // IPv6 address
				uint32_t        sin6_scope_id; // Scope ID (new in 2.4)
			};

			struct in6_addr {
				unsigned char   s6_addr[16];   // IPv6 address
			};
		*/
		var port uint16
		err = binary.Read(buff, binary.BigEndian, &port)
		if err != nil {
			return nil, fmt.Errorf("error parsing sockaddr_in6: %v", err)
		}
		res["sin6_port"] = strconv.Itoa(int(port))

		var flowinfo uint32
		err = binary.Read(buff, binary.BigEndian, &flowinfo)
		if err != nil {
			return nil, fmt.Errorf("error parsing sockaddr_in6: %v", err)
		}
		res["sin6_flowinfo"] = strconv.Itoa(int(flowinfo))
		addr, err := readByteSliceFromBuff(buff, 16)
		if err != nil {
			return nil, fmt.Errorf("error parsing sockaddr_in6: %v", err)
		}
		res["sin6_addr"] = Print16BytesSliceIP(addr)
		var scopeid uint32
		err = binary.Read(buff, binary.BigEndian, &scopeid)
		if err != nil {
			return nil, fmt.Errorf("error parsing sockaddr_in6: %v", err)
		}
		res["sin6_scopeid"] = strconv.Itoa(int(scopeid))
	}
	return res, nil
}

// alert struct encodes a security alert message with a timestamp
// it is used to unmarshal binary data and therefore should match (bit by bit) to the `alert_t` struct in the ebpf code.
type alert struct {
	Ts      uint64
	Msg     uint32
	Payload uint8
}

func readArgFromBuff(dataBuff io.Reader) (argTag, interface{}, error) {
	var err error
	var res interface{}
	var argTag argTag
	var argType argType
	err = binary.Read(dataBuff, binary.LittleEndian, &argType)
	if err != nil {
		return argTag, nil, fmt.Errorf("error reading arg type: %v", err)
	}
	err = binary.Read(dataBuff, binary.LittleEndian, &argTag)
	if err != nil {
		return argTag, nil, fmt.Errorf("error reading arg tag: %v", err)
	}
	switch argType {
	case intT:
		var data int32
		err = binary.Read(dataBuff, binary.LittleEndian, &data)
		res = data
	case uintT, devT, modeT:
		var data uint32
		err = binary.Read(dataBuff, binary.LittleEndian, &data)
		res = data
	case longT:
		var data int64
		err = binary.Read(dataBuff, binary.LittleEndian, &data)
		res = data
	case ulongT, offT, sizeT:
		var data uint64
		err = binary.Read(dataBuff, binary.LittleEndian, &data)
		res = data
	case pointerT:
		var data uint64
		err = binary.Read(dataBuff, binary.LittleEndian, &data)
		res = uintptr(data)
	case sockAddrT:
		res, err = readSockaddrFromBuff(dataBuff)
	case alertT:
		var data alert
		err = binary.Read(dataBuff, binary.LittleEndian, &data)
		res = data
	case strT:
		res, err = readStringFromBuff(dataBuff)
	case strArrT:
		var ss []string
		var arrLen uint8
		err = binary.Read(dataBuff, binary.LittleEndian, &arrLen)
		if err != nil {
			return argTag, nil, fmt.Errorf("error reading string array number of elements: %v", err)
		}
		for i := 0; i < int(arrLen); i++ {
			s, err := readStringFromBuff(dataBuff)
			if err != nil {
				return argTag, nil, fmt.Errorf("error reading string element: %v", err)
			}
			ss = append(ss, s)
		}
		res = ss
	default:
		// if we don't recognize the arg type, we can't parse the rest of the buffer
		return argTag, nil, fmt.Errorf("error unknown arg type %v", argType)
	}
	if err != nil {
		return argTag, nil, err
	}
	return argTag, res, nil
}
