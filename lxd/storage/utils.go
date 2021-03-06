package storage

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	"gopkg.in/robfig/cron.v2"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/rsync"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/ioprogress"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
	"github.com/lxc/lxd/shared/units"
)

// ValidName validates the provided name, and returns an error if it's not a valid storage name.
func ValidName(value string) error {
	if strings.Contains(value, "/") {
		return fmt.Errorf("Invalid storage volume name \"%s\". Storage volumes cannot contain \"/\" in their name", value)
	}

	return nil
}

// ConfigDiff returns a diff of the provided configs. Additionally, it returns whether or not
// only user properties have been changed.
func ConfigDiff(oldConfig map[string]string, newConfig map[string]string) ([]string, bool) {
	changedConfig := []string{}
	userOnly := true
	for key := range oldConfig {
		if oldConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	for key := range newConfig {
		if oldConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	// Skip on no change
	if len(changedConfig) == 0 {
		return nil, false
	}

	return changedConfig, userOnly
}

// VolumeTypeNameToType converts a volume type string to internal code.
func VolumeTypeNameToType(volumeTypeName string) (int, error) {
	switch volumeTypeName {
	case db.StoragePoolVolumeTypeNameContainer:
		return db.StoragePoolVolumeTypeContainer, nil
	case db.StoragePoolVolumeTypeNameVM:
		return db.StoragePoolVolumeTypeVM, nil
	case db.StoragePoolVolumeTypeNameImage:
		return db.StoragePoolVolumeTypeImage, nil
	case db.StoragePoolVolumeTypeNameCustom:
		return db.StoragePoolVolumeTypeCustom, nil
	}

	return -1, fmt.Errorf("Invalid storage volume type name")
}

// VolumeTypeToDBType converts volume type to internal code.
func VolumeTypeToDBType(volType drivers.VolumeType) (int, error) {
	switch volType {
	case drivers.VolumeTypeContainer:
		return db.StoragePoolVolumeTypeContainer, nil
	case drivers.VolumeTypeVM:
		return db.StoragePoolVolumeTypeVM, nil
	case drivers.VolumeTypeImage:
		return db.StoragePoolVolumeTypeImage, nil
	case drivers.VolumeTypeCustom:
		return db.StoragePoolVolumeTypeCustom, nil
	}

	return -1, fmt.Errorf("Invalid storage volume type")
}

// InstanceTypeToVolumeType converts instance type to volume type.
func InstanceTypeToVolumeType(instType instancetype.Type) (drivers.VolumeType, error) {
	switch instType {
	case instancetype.Container:
		return drivers.VolumeTypeContainer, nil
	case instancetype.VM:
		return drivers.VolumeTypeVM, nil
	}

	return "", fmt.Errorf("Invalid instance type")
}

// VolumeDBCreate creates a volume in the database.
func VolumeDBCreate(s *state.State, project, poolName, volumeName, volumeDescription, volumeTypeName string, snapshot bool, volumeConfig map[string]string, expiryDate time.Time) error {
	// Convert the volume type name to our internal integer representation.
	volumeType, err := VolumeTypeNameToType(volumeTypeName)
	if err != nil {
		return err
	}

	// Load storage pool the volume will be attached to.
	poolID, poolStruct, err := s.Cluster.StoragePoolGet(poolName)
	if err != nil {
		return err
	}

	// Check that a storage volume of the same storage volume type does not
	// already exist.
	volumeID, _ := s.Cluster.StoragePoolNodeVolumeGetTypeIDByProject(project, volumeName, volumeType, poolID)
	if volumeID > 0 {
		return fmt.Errorf("A storage volume of type %s already exists", volumeTypeName)
	}

	// Make sure that we don't pass a nil to the next function.
	if volumeConfig == nil {
		volumeConfig = map[string]string{}
	}

	// Validate the requested storage volume configuration.
	err = VolumeValidateConfig(s, poolName, volumeConfig, poolStruct)
	if err != nil {
		return err
	}

	err = VolumeFillDefault(poolName, volumeConfig, poolStruct)
	if err != nil {
		return err
	}

	// Create the database entry for the storage volume.
	if snapshot {
		_, err = s.Cluster.StoragePoolVolumeSnapshotCreate(project, volumeName, volumeDescription, volumeType, poolID, volumeConfig, expiryDate)
	} else {
		_, err = s.Cluster.StoragePoolVolumeCreate(project, volumeName, volumeDescription, volumeType, poolID, volumeConfig)
	}
	if err != nil {
		return fmt.Errorf("Error inserting %s of type %s into database: %s", poolName, volumeTypeName, err)
	}

	return nil
}

// SupportedPoolTypes the types of pools supported.
// Deprecated: this is being replaced with drivers.SupportedDrivers()
var SupportedPoolTypes = []string{"btrfs", "ceph", "cephfs", "dir", "lvm", "zfs"}

// StorageVolumeConfigKeys config validation for btrfs, ceph, cephfs, dir, lvm, zfs types.
// Deprecated: these are being moved to the per-storage-driver implementations.
var StorageVolumeConfigKeys = map[string]func(value string) ([]string, error){
	"block.filesystem": func(value string) ([]string, error) {
		err := shared.IsOneOf(value, []string{"btrfs", "ext4", "xfs"})
		if err != nil {
			return nil, err
		}

		return []string{"ceph", "lvm"}, nil
	},
	"block.mount_options": func(value string) ([]string, error) {
		return []string{"ceph", "lvm"}, shared.IsAny(value)
	},
	"security.shifted": func(value string) ([]string, error) {
		return SupportedPoolTypes, shared.IsBool(value)
	},
	"security.unmapped": func(value string) ([]string, error) {
		return SupportedPoolTypes, shared.IsBool(value)
	},
	"size": func(value string) ([]string, error) {
		if value == "" {
			return SupportedPoolTypes, nil
		}

		_, err := units.ParseByteSizeString(value)
		if err != nil {
			return nil, err
		}

		return SupportedPoolTypes, nil
	},
	"volatile.idmap.last": func(value string) ([]string, error) {
		return SupportedPoolTypes, shared.IsAny(value)
	},
	"volatile.idmap.next": func(value string) ([]string, error) {
		return SupportedPoolTypes, shared.IsAny(value)
	},
	"zfs.remove_snapshots": func(value string) ([]string, error) {
		err := shared.IsBool(value)
		if err != nil {
			return nil, err
		}

		return []string{"zfs"}, nil
	},
	"zfs.use_refquota": func(value string) ([]string, error) {
		err := shared.IsBool(value)
		if err != nil {
			return nil, err
		}

		return []string{"zfs"}, nil
	},
}

// VolumeValidateConfig validations volume config. Deprecated.
func VolumeValidateConfig(s *state.State, name string, config map[string]string, parentPool *api.StoragePool) error {
	logger := logging.AddContext(logger.Log, log.Ctx{"driver": parentPool.Driver, "pool": parentPool.Name})

	// Validate volume config using the new driver interface if supported.
	driver, err := drivers.Load(s, parentPool.Driver, parentPool.Name, parentPool.Config, logger, nil, commonRules())
	if err != drivers.ErrUnknownDriver {
		// Note: This legacy validation function doesn't have the concept of validating
		// different volumes types, so the types are hard coded as Custom and FS.
		return driver.ValidateVolume(drivers.NewVolume(driver, parentPool.Name, drivers.VolumeTypeCustom, drivers.ContentTypeFS, name, config, parentPool.Config), false)
	}

	// Otherwise fallback to doing legacy validation.
	for key, val := range config {
		// User keys are not validated.
		if strings.HasPrefix(key, "user.") {
			continue
		}

		// Validate storage volume config keys.
		validator, ok := StorageVolumeConfigKeys[key]
		if !ok {
			return fmt.Errorf("Invalid storage volume configuration key: %s", key)
		}

		_, err := validator(val)
		if err != nil {
			return err
		}

		if parentPool.Driver != "zfs" || parentPool.Driver == "dir" {
			if config["zfs.use_refquota"] != "" {
				return fmt.Errorf("the key volume.zfs.use_refquota cannot be used with non zfs storage volumes")
			}

			if config["zfs.remove_snapshots"] != "" {
				return fmt.Errorf("the key volume.zfs.remove_snapshots cannot be used with non zfs storage volumes")
			}
		}

		if parentPool.Driver == "dir" {
			if config["block.mount_options"] != "" {
				return fmt.Errorf("the key block.mount_options cannot be used with dir storage volumes")
			}

			if config["block.filesystem"] != "" {
				return fmt.Errorf("the key block.filesystem cannot be used with dir storage volumes")
			}
		}
	}

	return nil
}

// VolumeFillDefault fills default settings into a volume config.
func VolumeFillDefault(name string, config map[string]string, parentPool *api.StoragePool) error {
	if parentPool.Driver == "lvm" || parentPool.Driver == "ceph" {
		if config["block.filesystem"] == "" {
			config["block.filesystem"] = parentPool.Config["volume.block.filesystem"]
		}
		if config["block.filesystem"] == "" {
			// Unchangeable volume property: Set unconditionally.
			config["block.filesystem"] = drivers.DefaultFilesystem
		}

		if config["block.mount_options"] == "" {
			config["block.mount_options"] = parentPool.Config["volume.block.mount_options"]
		}
		if config["block.mount_options"] == "" {
			// Unchangeable volume property: Set unconditionally.
			config["block.mount_options"] = "discard"
		}
	}

	return nil
}

// VolumeSnapshotsGet returns a list of snapshots of the form <volume>/<snapshot-name>.
func VolumeSnapshotsGet(s *state.State, projectName string, pool string, volume string, volType int) ([]db.StorageVolumeArgs, error) {
	poolID, err := s.Cluster.StoragePoolGetID(pool)
	if err != nil {
		return nil, err
	}

	snapshots, err := s.Cluster.StoragePoolVolumeSnapshotsGetType(projectName, volume, volType, poolID)
	if err != nil {
		return nil, err
	}

	return snapshots, nil
}

// VolumePropertiesTranslate validates the supplied volume config and removes any keys that are not
// suitable for the volume's driver type.
func VolumePropertiesTranslate(targetConfig map[string]string, targetParentPoolDriver string) (map[string]string, error) {
	newConfig := make(map[string]string, len(targetConfig))
	for key, val := range targetConfig {
		// User keys are not validated.
		if strings.HasPrefix(key, "user.") {
			continue
		}

		// Validate storage volume config keys.
		validator, ok := StorageVolumeConfigKeys[key]
		if !ok {
			return nil, fmt.Errorf("Invalid storage volume configuration key: %s", key)
		}

		validStorageDrivers, err := validator(val)
		if err != nil {
			return nil, err
		}

		// Drop invalid keys.
		if !shared.StringInSlice(targetParentPoolDriver, validStorageDrivers) {
			continue
		}

		newConfig[key] = val
	}

	return newConfig, nil
}

// validatePoolCommonRules returns a map of pool config rules common to all drivers.
func validatePoolCommonRules() map[string]func(string) error {
	return map[string]func(string) error{
		"source":                  shared.IsAny,
		"volatile.initial_source": shared.IsAny,
		"volume.size":             shared.IsSize,
		"size":                    shared.IsSize,
		"rsync.bwlimit":           shared.IsAny,
	}
}

// validateVolumeCommonRules returns a map of volume config rules common to all drivers.
func validateVolumeCommonRules(vol drivers.Volume) map[string]func(string) error {
	rules := map[string]func(string) error{
		"volatile.idmap.last": shared.IsAny,
		"volatile.idmap.next": shared.IsAny,

		// Note: size should not be modifiable for non-custom volumes and should be checked
		// in the relevant volume update functions.
		"size": shared.IsSize,

		"snapshots.expiry": func(value string) error {
			// Validate expression
			_, err := shared.GetSnapshotExpiry(time.Time{}, value)
			return err
		},
		"snapshots.schedule": func(value string) error {
			if value == "" {
				return nil
			}

			if len(strings.Split(value, " ")) != 5 {
				return fmt.Errorf("Schedule must be of the form: <minute> <hour> <day-of-month> <month> <day-of-week>")
			}

			_, err := cron.Parse(fmt.Sprintf("* %s", value))
			if err != nil {
				return errors.Wrap(err, "Error parsing schedule")
			}

			return nil
		},
		"snapshots.pattern": shared.IsAny,
	}

	// block.mount_options is only relevant for drivers that are block backed and when there
	// is a filesystem to actually mount.
	if vol.IsBlockBacked() && vol.ContentType() == drivers.ContentTypeFS {
		rules["block.mount_options"] = shared.IsAny

		// Note: block.filesystem should not be modifiable after volume created. This should
		// be checked in the relevant volume update functions.
		rules["block.filesystem"] = shared.IsAny
	}

	// security.shifted and security.unmapped are only relevant for custom volumes.
	if vol.Type() == drivers.VolumeTypeCustom {
		rules["security.shifted"] = shared.IsBool
		rules["security.unmapped"] = shared.IsBool
	}

	return rules
}

// ImageUnpack unpacks a filesystem image into the destination path.
// There are several formats that images can come in:
// Container Format A: Separate metadata tarball and root squashfs file.
// 	- Unpack metadata tarball into mountPath.
//	- Unpack root squashfs file into mountPath/rootfs.
// Container Format B: Combined tarball containing metadata files and root squashfs.
//	- Unpack combined tarball into mountPath.
// VM Format A: Separate metadata tarball and root qcow2 file.
// 	- Unpack metadata tarball into mountPath.
//	- Check rootBlockPath is a file and convert qcow2 file into raw format in rootBlockPath.
func ImageUnpack(imageFile, destPath, destBlockFile string, blockBackend, runningInUserns bool, tracker *ioprogress.ProgressTracker) error {
	// For all formats, first unpack the metadata (or combined) tarball into destPath.
	imageRootfsFile := imageFile + ".rootfs"

	// If no destBlockFile supplied then this is a container image unpack.
	if destBlockFile == "" {
		rootfsPath := filepath.Join(destPath, "rootfs")

		// Unpack the main image file.
		err := shared.Unpack(imageFile, destPath, blockBackend, runningInUserns, tracker)
		if err != nil {
			return err
		}

		// Check for separate root file.
		if shared.PathExists(imageRootfsFile) {
			err = os.MkdirAll(rootfsPath, 0755)
			if err != nil {
				return fmt.Errorf("Error creating rootfs directory")
			}

			err = shared.Unpack(imageRootfsFile, rootfsPath, blockBackend, runningInUserns, tracker)
			if err != nil {
				return err
			}
		}

		// Check that the container image unpack has resulted in a rootfs dir.
		if !shared.PathExists(rootfsPath) {
			return fmt.Errorf("Image is missing a rootfs: %s", imageFile)
		}

		// Done with this.
		return nil
	}

	// If a rootBlockPath is supplied then this is a VM image unpack.

	// Validate the target.
	fileInfo, err := os.Stat(destBlockFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if fileInfo != nil && fileInfo.IsDir() {
		// If the dest block file exists, and it is a directory, fail.
		return fmt.Errorf("Root block path isn't a file: %s", destBlockFile)
	}

	if shared.PathExists(imageRootfsFile) {
		// Unpack the main image file.
		err := shared.Unpack(imageFile, destPath, blockBackend, runningInUserns, tracker)
		if err != nil {
			return err
		}

		// Convert the qcow2 format to a raw block device.
		_, err = shared.RunCommand("qemu-img", "convert", "-O", "raw", imageRootfsFile, destBlockFile)
		if err != nil {
			return fmt.Errorf("Failed converting image to raw at %s: %v", destBlockFile, err)
		}
	} else {
		// Dealing with unified tarballs require an initial unpack to a temporary directory.
		tempDir, err := ioutil.TempDir(shared.VarPath("images"), "lxd_image_unpack_")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tempDir)

		// Unpack the whole image.
		err = shared.Unpack(imageFile, tempDir, blockBackend, runningInUserns, tracker)
		if err != nil {
			return err
		}

		// Convert the qcow2 format to a raw block device.
		imgPath := filepath.Join(tempDir, "rootfs.img")
		_, err = shared.RunCommand("qemu-img", "convert", "-O", "raw", imgPath, destBlockFile)
		if err != nil {
			return fmt.Errorf("Failed converting image to raw at %s: %v", destBlockFile, err)
		}

		// Delete the qcow2.
		err = os.Remove(imgPath)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove %q", imgPath)
		}

		// Transfer the content.
		_, err = rsync.LocalCopy(tempDir, destPath, "", true)
		if err != nil {
			return err
		}
	}

	return nil
}

// InstanceContentType returns the instance's content type.
func InstanceContentType(inst instance.Instance) drivers.ContentType {
	contentType := drivers.ContentTypeFS
	if inst.Type() == instancetype.VM {
		contentType = drivers.ContentTypeBlock
	}

	return contentType
}

// VolumeUsedByInstancesGet gets a list of instance names using a volume.
func VolumeUsedByInstancesGet(s *state.State, projectName string, poolName string, volumeName string) ([]string, error) {
	insts, err := instance.LoadByProject(s, projectName)
	if err != nil {
		return []string{}, err
	}

	instUsingVolume := []string{}
	for _, inst := range insts {
		for _, dev := range inst.LocalDevices() {
			if dev["type"] != "disk" {
				continue
			}

			if dev["pool"] == poolName && dev["source"] == volumeName {
				instUsingVolume = append(instUsingVolume, inst.Name())
				break
			}
		}
	}

	return instUsingVolume, nil
}

// VolumeUsedByRunningInstancesWithProfilesGet gets list of running instances using a volume.
func VolumeUsedByRunningInstancesWithProfilesGet(s *state.State, projectName string, poolName string, volumeName string, volumeTypeName string, runningOnly bool) ([]string, error) {
	insts, err := instance.LoadByProject(s, projectName)
	if err != nil {
		return []string{}, err
	}

	instUsingVolume := []string{}
	volumeNameWithType := fmt.Sprintf("%s/%s", volumeTypeName, volumeName)
	for _, inst := range insts {
		if runningOnly && !inst.IsRunning() {
			continue
		}

		for _, dev := range inst.ExpandedDevices() {
			if dev["type"] != "disk" {
				continue
			}

			if dev["pool"] != poolName {
				continue
			}

			// Make sure that we don't compare against stuff like
			// "container////bla" but only against "container/bla".
			cleanSource := filepath.Clean(dev["source"])
			if cleanSource == volumeName || cleanSource == volumeNameWithType {
				instUsingVolume = append(instUsingVolume, inst.Name())
			}
		}
	}

	return instUsingVolume, nil
}

// VolumeUsedByDaemon indicates whether the volume is used by daemon storage.
func VolumeUsedByDaemon(s *state.State, poolName string, volumeName string) (bool, error) {
	var storageBackups string
	var storageImages string
	err := s.Node.Transaction(func(tx *db.NodeTx) error {
		nodeConfig, err := node.ConfigLoad(tx)
		if err != nil {
			return err
		}

		storageBackups = nodeConfig.StorageBackupsVolume()
		storageImages = nodeConfig.StorageImagesVolume()

		return nil
	})
	if err != nil {
		return false, err
	}

	fullName := fmt.Sprintf("%s/%s", poolName, volumeName)
	if storageBackups == fullName || storageImages == fullName {
		return true, nil
	}

	return false, nil
}

// FallbackMigrationType returns the fallback migration transport to use based on volume content type.
func FallbackMigrationType(contentType drivers.ContentType) migration.MigrationFSType {
	if contentType == drivers.ContentTypeBlock {
		return migration.MigrationFSType_BLOCK_AND_RSYNC
	}

	return migration.MigrationFSType_RSYNC
}

// RenderSnapshotUsage can be used as an optional argument to Instance.Render() to return snapshot usage.
// As this is a relatively expensive operation it is provided as an optional feature rather than on by default.
func RenderSnapshotUsage(s *state.State, snapInst instance.Instance) func(response interface{}) error {
	return func(response interface{}) error {
		apiRes, ok := response.(*api.InstanceSnapshot)
		if !ok {
			return nil
		}

		pool, err := GetPoolByInstance(s, snapInst)
		if err == nil {
			// It is important that the snapshot not be mounted here as mounting a snapshot can trigger a very
			// expensive filesystem UUID regeneration, so we rely on the driver implementation to get the info
			// we are requesting as cheaply as possible.
			apiRes.Size, _ = pool.GetInstanceUsage(snapInst)
		}

		return nil
	}
}
