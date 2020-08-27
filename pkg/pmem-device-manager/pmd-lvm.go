package pmdmanager

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	api "github.com/intel/pmem-csi/pkg/apis/pmemcsi/v1alpha1"
	"github.com/intel/pmem-csi/pkg/ndctl"
	pmemcommon "github.com/intel/pmem-csi/pkg/pmem-common"
	pmemexec "github.com/intel/pmem-csi/pkg/pmem-exec"
	"k8s.io/klog"
)

const (
	// 4 MB alignment is used by LVM
	lvmAlign uint64 = 4 * 1024 * 1024
)

type pmemLvm struct {
	volumeGroups []string
	devices      map[string]*PmemDeviceInfo
}

var _ PmemDeviceManager = &pmemLvm{}
var lvsArgs = []string{"--noheadings", "--nosuffix", "-o", "lv_name,lv_path,lv_size", "--units", "B"}
var vgsArgs = []string{"--noheadings", "--nosuffix", "-o", "vg_name,vg_size,vg_free", "--units", "B"}

// mutex to synchronize all LVM calls
// The reason we chose not to support concurrent LVM calls was
// due to LVM's inconsistent behavior when made concurrent calls
// in our stress tests. One should revisit this and choose better
// suitable synchronization policy.
var lvmMutex = &sync.Mutex{}

// NewPmemDeviceManagerLVM Instantiates a new LVM based pmem device manager
func NewPmemDeviceManagerLVM(pmemPercentage uint) (PmemDeviceManager, error) {
	if pmemPercentage > 100 {
		return nil, fmt.Errorf("invalid pmemPercentage '%d'. Value must be 0..100", pmemPercentage)
	}
	lvmMutex.Lock()
	defer lvmMutex.Unlock()

	ndctx, err := ndctl.NewContext()
	if err != nil {
		return nil, err
	}
	defer ndctx.Free()

	volumeGroups := []string{}
	for _, bus := range ndctx.GetBuses() {
		for _, r := range bus.ActiveRegions() {
			vgName := pmemcommon.VgName(bus, r)
			if r.Type() != ndctl.PmemRegion {
				klog.Infof("Region is not suitable for fsdax, skipping it: id = %q, device %q", r.ID(), r.DeviceName())
				continue
			}

			if err := setupNS(r, pmemPercentage); err != nil {
				return nil, err
			}
			if err := setupVG(r, vgName); err != nil {
				return nil, err
			}
			if _, err := pmemexec.RunCommand("vgs", vgName); err != nil {
				klog.V(5).Infof("NewPmemDeviceManagerLVM: VG %v non-existent, skip", vgName)
			} else {
				volumeGroups = append(volumeGroups, vgName)
			}
		}
	}

	return NewPmemDeviceManagerLVMForVGs(volumeGroups)
}

func (pmem *pmemLvm) GetMode() api.DeviceMode {
	return api.DeviceModeLVM
}

func NewPmemDeviceManagerLVMForVGs(volumeGroups []string) (PmemDeviceManager, error) {
	devices, err := listDevices(volumeGroups...)
	if err != nil {
		return nil, err
	}

	return &pmemLvm{
		volumeGroups: volumeGroups,
		devices:      devices,
	}, nil
}

type vgInfo struct {
	name string
	size uint64
	free uint64
}

func (lvm *pmemLvm) GetCapacity() (capacity Capacity, err error) {
	lvmMutex.Lock()
	defer lvmMutex.Unlock()

	var vgs []vgInfo
	vgs, err = getVolumeGroups(lvm.volumeGroups)
	if err != nil {
		return
	}

	for _, vg := range vgs {
		if vg.free > capacity.MaxVolumeSize {
			// TODO: alignment?
			capacity.MaxVolumeSize = vg.free
		}
		capacity.Available += vg.free
		capacity.Managed += vg.size
		capacity.Total, err = totalSize()
		if err != nil {
			return
		}
	}

	return capacity, nil
}

func (lvm *pmemLvm) CreateDevice(volumeId string, size uint64) error {
	lvmMutex.Lock()
	defer lvmMutex.Unlock()
	// Check that such volume does not exist. In certain error states, for example when
	// namespace creation works but device zeroing fails (missing /dev/pmemX.Y in container),
	// this function is asked to create new devices repeatedly, forcing running out of space.
	// Avoid device filling with garbage entries by returning error.
	// Overall, no point having more than one namespace with same volumeId.
	if _, err := lvm.getDevice(volumeId); err == nil {
		return ErrDeviceExists
	}
	vgs, err := getVolumeGroups(lvm.volumeGroups)
	if err != nil {
		return err
	}
	// Adjust up to next alignment boundary, if not aligned already.
	// This logic relies on size guaranteed to be nonzero,
	// which is now achieved by check in upper layer.
	// If zero size possible then we would need to check and increment by lvmAlign,
	// because LVM does not tolerate creation of zero size.
	if reminder := size % lvmAlign; reminder != 0 {
		klog.V(5).Infof("CreateDevice align size up by %v: from %v", lvmAlign-reminder, size)
		size += lvmAlign - reminder
		klog.V(5).Infof("CreateDevice align size up: to %v", size)
	}
	strSz := strconv.FormatUint(size, 10) + "B"

	for _, vg := range vgs {
		// use first Vgroup with enough available space
		if vg.free >= size {
			// In some container environments clearing device fails with race condition.
			// So, we ask lvm not to clear(-Zn) the newly created device, instead we do ourself in later stage.
			// lvcreate takes size in MBytes if no unit
			if _, err := pmemexec.RunCommand("lvcreate", "-Zn", "-L", strSz, "-n", volumeId, vg.name); err != nil {
				klog.V(3).Infof("lvcreate failed with error: %v, trying for next free region", err)
			} else {
				// clear start of device to avoid old data being recognized as file system
				device, err := getUncachedDevice(volumeId, vg.name)
				if err != nil {
					return err
				}
				if err := waitDeviceAppears(device); err != nil {
					return err
				}
				if err := clearDevice(device, false); err != nil {
					return fmt.Errorf("clear device %q: %v", volumeId, err)
				}

				lvm.devices[device.VolumeId] = device

				return nil
			}
		}
	}
	return ErrNotEnoughSpace
}

func (lvm *pmemLvm) DeleteDevice(volumeId string, flush bool) error {
	lvmMutex.Lock()
	defer lvmMutex.Unlock()

	var err error
	var device *PmemDeviceInfo

	if device, err = lvm.getDevice(volumeId); err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			return nil
		}
		return err
	}
	if err := clearDevice(device, flush); err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			// Remove device from cache
			delete(lvm.devices, volumeId)
			return nil
		}
		return err
	}

	if _, err := pmemexec.RunCommand("lvremove", "-fy", device.Path); err != nil {
		return err
	}

	// Remove device from cache
	delete(lvm.devices, volumeId)

	return nil
}

func (lvm *pmemLvm) ListDevices() ([]*PmemDeviceInfo, error) {
	lvmMutex.Lock()
	defer lvmMutex.Unlock()

	devices := []*PmemDeviceInfo{}
	for _, dev := range lvm.devices {
		devices = append(devices, dev)
	}

	return devices, nil
}

func (lvm *pmemLvm) GetDevice(volumeId string) (*PmemDeviceInfo, error) {
	lvmMutex.Lock()
	defer lvmMutex.Unlock()

	return lvm.getDevice(volumeId)
}

func (lvm *pmemLvm) getDevice(volumeId string) (*PmemDeviceInfo, error) {
	if dev, ok := lvm.devices[volumeId]; ok {
		return dev, nil
	}

	return nil, ErrDeviceNotFound
}

func getUncachedDevice(volumeId string, volumeGroup string) (*PmemDeviceInfo, error) {
	devices, err := listDevices(volumeGroup)
	if err != nil {
		return nil, err
	}

	if dev, ok := devices[volumeId]; ok {
		return dev, nil
	}

	return nil, ErrDeviceNotFound
}

// listDevices Lists available logical devices in given volume groups
func listDevices(volumeGroups ...string) (map[string]*PmemDeviceInfo, error) {
	args := append(lvsArgs, volumeGroups...)
	output, err := pmemexec.RunCommand("lvs", args...)
	if err != nil {
		return nil, fmt.Errorf("lvs failure : %v", err)
	}
	return parseLVSOutput(output)
}

//lvs options "lv_name,lv_path,lv_size,lv_free"
func parseLVSOutput(output string) (map[string]*PmemDeviceInfo, error) {
	devices := map[string]*PmemDeviceInfo{}
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 3 {
			continue
		}

		dev := &PmemDeviceInfo{}
		dev.VolumeId = fields[0]
		dev.Path = fields[1]
		dev.Size, _ = strconv.ParseUint(fields[2], 10, 64)

		devices[dev.VolumeId] = dev
	}

	return devices, nil
}

func getVolumeGroups(groups []string) ([]vgInfo, error) {
	vgs := []vgInfo{}
	args := append(vgsArgs, groups...)
	output, err := pmemexec.RunCommand("vgs", args...)
	if err != nil {
		return vgs, fmt.Errorf("vgs failure: %v", err)
	}
	for _, line := range strings.SplitN(output, "\n", len(groups)) {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 3 {
			return vgs, fmt.Errorf("failed to parse vgs output: %q", line)
		}
		vg := vgInfo{}
		vg.name = fields[0]
		vg.size, _ = strconv.ParseUint(fields[1], 10, 64)
		vg.free, _ = strconv.ParseUint(fields[2], 10, 64)
		vgs = append(vgs, vg)
	}

	return vgs, nil
}

const (
	GB uint64 = 1024 * 1024 * 1024
)

// setupNS checks if a namespace needs to be created in the region and if so, does that.
func setupNS(r *ndctl.Region, percentage uint) error {
	align := GB
	realalign := align * r.InterleaveWays()
	canUse := uint64(percentage) * (r.Size() / 100)
	klog.V(3).Infof("Create fsdax-namespaces in %v, allowed %d %%, real align %d:\ntotal       : %16d\navail       : %16d\ncan use     : %16d",
		r.DeviceName(), percentage, realalign, r.Size(), r.AvailableSize(), canUse)
	// Subtract sizes of existing active namespaces with currently handled mode and owned by pmem-csi
	for _, ns := range r.ActiveNamespaces() {
		klog.V(5).Infof("createNS: Exists: Size %16d Mode:%v Device:%v Name:%v", ns.Size(), ns.Mode(), ns.DeviceName(), ns.Name())
		if ns.Name() != "pmem-csi" {
			continue
		}
		diff := int64(canUse - ns.Size())
		if diff <= 0 {
			canUse = 0
		} else {
			canUse = uint64(diff)
		}
	}
	klog.V(4).Infof("Calculated canUse:%v, available by Region info:%v", canUse, r.AvailableSize())
	// Because of overhead by alignment and extra space for page mapping, calculated available may show more than actual
	if r.AvailableSize() < canUse {
		klog.V(4).Infof("Available in Region:%v is less than desired size, limit to that", r.AvailableSize())
		canUse = r.AvailableSize()
	}
	// Should not happen often: fragmented space could lead to r.MaxAvailableExtent() being less than r.AvailableSize()
	if r.MaxAvailableExtent() < canUse {
		klog.V(4).Infof("MaxAvailableExtent in Region:%v is less than desired size, limit to that", r.MaxAvailableExtent())
		canUse = r.MaxAvailableExtent()
	}
	// Align down to next real alignment boundary, as trying creation above it may fail.
	canUse /= realalign
	canUse *= realalign
	// If less than 2GB usable, don't attempt as creation would fail
	minsize := 2 * GB
	if canUse >= minsize {
		klog.V(3).Infof("Create %v-bytes fsdax-namespace", canUse)
		_, err := r.CreateNamespace(ndctl.CreateNamespaceOpts{
			Name:  "pmem-csi",
			Mode:  "fsdax",
			Size:  canUse,
			Align: align,
		})
		if err != nil {
			return fmt.Errorf("failed to create PMEM namespace with size '%d' in region '%s': %v", canUse, r.DeviceName(), err)
		}
	}

	return nil
}

// setupVG ensures that all namespaces with name "pmem-csi" in the region
// are part of the volume group.
func setupVG(r *ndctl.Region, vgName string) error {
	nsArray := r.ActiveNamespaces()
	if len(nsArray) == 0 {
		klog.V(3).Infof("No active namespaces in region %s", r.DeviceName())
		return nil
	}
	var devNames []string
	for _, ns := range nsArray {
		// consider only namespaces having name given by this driver, to exclude foreign ones
		if ns.Name() == "pmem-csi" {
			devName := "/dev/" + ns.BlockDeviceName()
			/* check if this pv is already part of a group, if yes ignore this pv
			if not add to arg list */
			output, err := pmemexec.RunCommand("pvs", "--noheadings", "-o", "vg_name", devName)
			if err != nil || len(strings.TrimSpace(output)) == 0 {
				devNames = append(devNames, devName)
			}
		}
	}
	if len(devNames) == 0 {
		klog.V(3).Infof("no new namespace found to add to this group: %s", vgName)
		return nil
	}

	cmd := ""
	if _, err := pmemexec.RunCommand("vgdisplay", vgName); err != nil {
		klog.V(3).Infof("No volume group with name %v, mark for creation", vgName)
		cmd = "vgcreate"
	} else {
		klog.V(3).Infof("VolGroup '%v' exists", vgName)
		cmd = "vgextend"
	}

	cmdArgs := []string{"--force", vgName}
	cmdArgs = append(cmdArgs, devNames...)
	_, err := pmemexec.RunCommand(cmd, cmdArgs...) //nolint gosec
	if err != nil {
		return fmt.Errorf("failed to create/extend volume group '%s': %v", vgName, err)
	}
	return nil
}
