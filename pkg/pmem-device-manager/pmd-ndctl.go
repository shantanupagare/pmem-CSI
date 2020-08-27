package pmdmanager

import (
	"errors"
	"fmt"
	"sync"

	api "github.com/intel/pmem-csi/pkg/apis/pmemcsi/v1alpha1"
	"github.com/intel/pmem-csi/pkg/ndctl"
	"k8s.io/klog"
	"k8s.io/utils/mount"
)

const (
	// 1 GB align in ndctl creation request has proven to be reliable.
	// Newer kernels may allow smaller alignment but we do not want to introduce kernel dependency.
	ndctlAlign uint64 = 1024 * 1024 * 1024
)

type pmemNdctl struct {
	pmemPercentage uint
}

var _ PmemDeviceManager = &pmemNdctl{}

// mutex to synchronize all ndctl calls
// https://github.com/pmem/ndctl/issues/96
// Once ndctl supports concurrent calls we need to revisit
// our locking strategy.
var ndctlMutex = &sync.Mutex{}

//NewPmemDeviceManagerNdctl Instantiates a new ndctl based pmem device manager
// FIXME(avalluri): consider pmemPercentage while calculating available space
func NewPmemDeviceManagerNdctl(pmemPercentage uint) (PmemDeviceManager, error) {
	if pmemPercentage > 100 {
		return nil, fmt.Errorf("invalid pmemPercentage '%d'. Value must be 0..100", pmemPercentage)
	}
	// Check is /sys writable. If not then there is no point starting
	mounts, _ := mount.New("").List()
	for _, mnt := range mounts {
		if mnt.Device == "sysfs" && mnt.Path == "/sys" {
			klog.V(5).Infof("NewPmemDeviceManagerNdctl: sysfs mount options:%s", mnt.Opts)
			for _, opt := range mnt.Opts {
				if opt == "rw" {
					klog.V(4).Info("NewPmemDeviceManagerNdctl: /sys mounted read-write, good")
					return &pmemNdctl{pmemPercentage: pmemPercentage}, nil
				} else if opt == "ro" {
					return nil, fmt.Errorf("FATAL: /sys mounted read-only, can not operate")
				}
			}
			return nil, fmt.Errorf("FATAL: /sys mount entry exists but does not have neither 'rw' or 'ro' option")
		}
	}
	return nil, fmt.Errorf("FATAL: /sys mount entry not present")
}

func (pmem *pmemNdctl) GetMode() api.DeviceMode {
	return api.DeviceModeDirect
}

func (pmem *pmemNdctl) GetCapacity() (capacity Capacity, err error) {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	var ndctx *ndctl.Context
	ndctx, err = ndctl.NewContext()
	if err != nil {
		return
	}
	defer ndctx.Free()

	for _, bus := range ndctx.GetBuses() {
		for _, r := range bus.AllRegions() {
			capacity.Total += r.Size()
			// TODO: check type?!
			if !r.Enabled() {
				continue
			}

			realalign := ndctlAlign * r.InterleaveWays()
			available := r.MaxAvailableExtent()
			// align down, avoid claiming more than what we really can serve
			klog.V(4).Infof("GetCapacity: available before realalign: %d", available)
			available /= realalign
			available *= realalign
			klog.V(4).Infof("GetCapacity: available after realalign: %d", available)
			if available > capacity.MaxVolumeSize {
				capacity.MaxVolumeSize = available
			}
			capacity.Available += available
			capacity.Managed += r.Size()
		}
	}
	// TODO: we should maintain capacity when adding or subtracting
	// from upper layer, not done right now!!
	return capacity, nil
}

func (pmem *pmemNdctl) CreateDevice(volumeId string, size uint64) error {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	ndctx, err := ndctl.NewContext()
	if err != nil {
		return err
	}
	defer ndctx.Free()

	// Check that such volume does not exist. In certain error states, for example when
	// namespace creation works but device zeroing fails (missing /dev/pmemX.Y in container),
	// this function is asked to create new devices repeatedly, forcing running out of space.
	// Avoid device filling with garbage entries by returning error.
	// Overall, no point having more than one namespace with same name.
	if _, err := getDevice(ndctx, volumeId); err == nil {
		return ErrDeviceExists
	}

	// libndctl needs to store meta data and will use some of the allocated
	// space for that (https://github.com/pmem/ndctl/issues/79).
	// We don't know exactly how much space that is, just
	// that it should be a small amount. But because libndctl
	// rounds up to the alignment, in practice that means we need
	// to request `align` additional bytes.
	size += ndctlAlign
	klog.V(4).Infof("Compensate for libndctl creating one alignment step smaller: increase size to %d", size)
	ns, err := ndctx.CreateNamespace(ndctl.CreateNamespaceOpts{
		Name:  volumeId,
		Size:  size,
		Align: ndctlAlign,
		Mode:  "fsdax",
	})
	if err != nil {
		return err
	}
	data, _ := ns.MarshalJSON() //nolint: gosec
	klog.V(3).Infof("Namespace created: %s", data)
	// clear start of device to avoid old data being recognized as file system
	device, err := getDevice(ndctx, volumeId)
	if err != nil {
		return err
	}
	if err := clearDevice(device, false); err != nil {
		return fmt.Errorf("clear device %q: %v", volumeId, err)
	}

	return nil
}

func (pmem *pmemNdctl) DeleteDevice(volumeId string, flush bool) error {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	ndctx, err := ndctl.NewContext()
	if err != nil {
		return err
	}
	defer ndctx.Free()

	device, err := getDevice(ndctx, volumeId)
	if err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			return nil
		}
		return err
	}
	if err := clearDevice(device, flush); err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			return nil
		}
		return err
	}
	return ndctx.DestroyNamespaceByName(volumeId)
}

func (pmem *pmemNdctl) GetDevice(volumeId string) (*PmemDeviceInfo, error) {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	ndctx, err := ndctl.NewContext()
	if err != nil {
		return nil, err
	}
	defer ndctx.Free()

	return getDevice(ndctx, volumeId)
}

func (pmem *pmemNdctl) ListDevices() ([]*PmemDeviceInfo, error) {
	ndctlMutex.Lock()
	defer ndctlMutex.Unlock()

	ndctx, err := ndctl.NewContext()
	if err != nil {
		return nil, err
	}
	defer ndctx.Free()

	devices := []*PmemDeviceInfo{}
	for _, ns := range ndctx.GetAllNamespaces() {
		devices = append(devices, namespaceToPmemInfo(ns))
	}
	return devices, nil
}

func getDevice(ndctx *ndctl.Context, volumeId string) (*PmemDeviceInfo, error) {
	ns, err := ndctx.GetNamespaceByName(volumeId)
	if err != nil {
		if errors.Is(err, ndctl.ErrNotExist) {
			return nil, ErrDeviceNotFound
		}
		return nil, fmt.Errorf("error getting device %q: %v", volumeId, err)
	}

	return namespaceToPmemInfo(ns), nil
}

func namespaceToPmemInfo(ns *ndctl.Namespace) *PmemDeviceInfo {
	return &PmemDeviceInfo{
		VolumeId: ns.Name(),
		Path:     "/dev/" + ns.BlockDeviceName(),
		Size:     ns.Size(),
	}
}

// totalSize sums up all PMEM regions, regardless whether they are
// enabled and regardless of their mode.
func totalSize() (size uint64, err error) {
	var ndctx *ndctl.Context
	ndctx, err = ndctl.NewContext()
	if err != nil {
		return
	}
	defer ndctx.Free()

	for _, bus := range ndctx.GetBuses() {
		for _, region := range bus.AllRegions() {
			size += region.Size()
		}
	}
	return
}
