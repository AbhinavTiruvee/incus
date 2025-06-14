package drivers

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/lxc/incus/v6/internal/linux"
	"github.com/lxc/incus/v6/internal/migration"
	deviceConfig "github.com/lxc/incus/v6/internal/server/device/config"
	localMigration "github.com/lxc/incus/v6/internal/server/migration"
	"github.com/lxc/incus/v6/internal/server/operations"
	internalUtil "github.com/lxc/incus/v6/internal/util"
	"github.com/lxc/incus/v6/internal/version"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/logger"
	"github.com/lxc/incus/v6/shared/revert"
	"github.com/lxc/incus/v6/shared/subprocess"
	"github.com/lxc/incus/v6/shared/units"
	"github.com/lxc/incus/v6/shared/util"
	"github.com/lxc/incus/v6/shared/validate"
)

const zfsDefaultVdevType = "stripe"

var zfsSupportedVdevTypes = []string{
	zfsDefaultVdevType,
	"mirror",
	"raidz1",
	"raidz2",
}

var (
	zfsVersion  string
	zfsLoaded   bool
	zfsDirectIO bool
	zfsTrim     bool
	zfsRaw      bool
	zfsDelegate bool
)

var zfsDefaultSettings = map[string]string{
	"relatime":   "on",
	"mountpoint": "legacy",
	"setuid":     "on",
	"exec":       "on",
	"devices":    "on",
	"acltype":    "posixacl",
	"xattr":      "sa",
}

type zfs struct {
	common

	// Temporary cache (typically lives for the duration of a query).
	cache   map[string]map[string]int64
	cacheMu sync.Mutex
}

// load is used to run one-time action per-driver rather than per-pool.
func (d *zfs) load() error {
	// Register the patches.
	d.patches = map[string]func() error{
		"storage_lvm_skipactivation":                         nil,
		"storage_missing_snapshot_records":                   nil,
		"storage_delete_old_snapshot_records":                nil,
		"storage_zfs_drop_block_volume_filesystem_extension": d.patchDropBlockVolumeFilesystemExtension,
		"storage_prefix_bucket_names_with_project":           nil,
	}

	// Done if previously loaded.
	if zfsLoaded {
		return nil
	}

	// Validate the needed tools are present.
	for _, tool := range []string{"zpool", "zfs"} {
		_, err := exec.LookPath(tool)
		if err != nil {
			return fmt.Errorf("Required tool '%s' is missing", tool)
		}
	}

	// Load the kernel module.
	err := linux.LoadModule("zfs")
	if err != nil {
		return fmt.Errorf("Error loading %q module: %w", "zfs", err)
	}

	// Get the version information.
	if zfsVersion == "" {
		version, err := d.version()
		if err != nil {
			return err
		}

		zfsVersion = version
	}

	// Decide whether we can use features added by 0.8.0.
	ver080, err := version.Parse("0.8.0")
	if err != nil {
		return err
	}

	ourVer, err := version.Parse(zfsVersion)
	if err != nil {
		return err
	}

	// If running 0.8.0 or newer, we can use direct I/O, trim and raw.
	if ourVer.Compare(ver080) >= 0 {
		zfsDirectIO = true
		zfsTrim = true
		zfsRaw = true
	}

	// Detect support for ZFS delegation.
	ver220, err := version.Parse("2.2.0")
	if err != nil {
		return err
	}

	if ourVer.Compare(ver220) >= 0 {
		zfsDelegate = true
	}

	zfsLoaded = true
	return nil
}

// Info returns info about the driver and its environment.
func (d *zfs) Info() Info {
	info := Info{
		Name:                         "zfs",
		Version:                      zfsVersion,
		DefaultVMBlockFilesystemSize: deviceConfig.DefaultVMBlockFilesystemSize,
		OptimizedImages:              true,
		OptimizedBackups:             true,
		PreservesInodes:              true,
		Remote:                       d.isRemote(),
		VolumeTypes:                  []VolumeType{VolumeTypeBucket, VolumeTypeCustom, VolumeTypeImage, VolumeTypeContainer, VolumeTypeVM},
		VolumeMultiNode:              d.isRemote(),
		BlockBacking:                 util.IsTrue(d.config["volume.zfs.block_mode"]),
		RunningCopyFreeze:            false,
		DirectIO:                     zfsDirectIO,
		MountedRoot:                  false,
		Buckets:                      true,
	}

	return info
}

// ensureInitialDatasets creates missing initial datasets or configures existing ones with current policy.
// Accepts warnOnExistingPolicyApplyError argument, if true will warn rather than fail if applying current policy
// to an existing dataset fails.
func (d zfs) ensureInitialDatasets(warnOnExistingPolicyApplyError bool) error {
	// Build the list of datasets to query.
	datasets := []string{d.config["zfs.pool_name"]}
	for _, entry := range d.initialDatasets() {
		datasets = append(datasets, filepath.Join(d.config["zfs.pool_name"], entry))
	}

	// Build the list of properties to check.
	props := []string{"name", "mountpoint", "volmode"}
	for k := range zfsDefaultSettings {
		props = append(props, k)
	}

	// Get current state.
	args := append([]string{"get", "-H", "-p", "-o", "name,property,value", strings.Join(props, ",")}, datasets...)
	output, _ := subprocess.RunCommand("zfs", args...)

	currentConfig := map[string]map[string]string{}
	for _, entry := range strings.Split(output, "\n") {
		if entry == "" {
			continue
		}

		fields := strings.Fields(entry)
		if len(fields) != 3 {
			continue
		}

		if currentConfig[fields[0]] == nil {
			currentConfig[fields[0]] = map[string]string{}
		}

		currentConfig[fields[0]][fields[1]] = fields[2]
	}

	// Check that the root dataset is correctly configured.
	args = []string{}
	for k, v := range zfsDefaultSettings {
		current := currentConfig[d.config["zfs.pool_name"]][k]
		if current == v {
			continue
		}

		// Workaround for values having been renamed over time.
		if k == "acltype" && current == "posix" {
			continue
		}

		if k == "xattr" && current == "on" {
			continue
		}

		args = append(args, fmt.Sprintf("%s=%s", k, v))
	}

	if len(args) > 0 {
		err := d.setDatasetProperties(d.config["zfs.pool_name"], args...)
		if err != nil {
			if !warnOnExistingPolicyApplyError {
				return fmt.Errorf("Failed applying policy to existing dataset %q: %w", d.config["zfs.pool_name"], err)
			}

			d.logger.Warn("Failed applying policy to existing dataset", logger.Ctx{"dataset": d.config["zfs.pool_name"], "err": err})
		}
	}

	// Check the initial datasets.
	for _, dataset := range d.initialDatasets() {
		properties := map[string]string{"mountpoint": "legacy"}
		if slices.Contains([]string{"virtual-machines", "deleted/virtual-machines"}, dataset) {
			properties["volmode"] = "none"
		}

		datasetPath := filepath.Join(d.config["zfs.pool_name"], dataset)
		if currentConfig[datasetPath] != nil {
			args := []string{}
			for k, v := range properties {
				if currentConfig[datasetPath][k] == v {
					continue
				}

				args = append(args, fmt.Sprintf("%s=%s", k, v))
			}

			if len(args) > 0 {
				err := d.setDatasetProperties(datasetPath, args...)
				if err != nil {
					if !warnOnExistingPolicyApplyError {
						return fmt.Errorf("Failed applying policy to existing dataset %q: %w", datasetPath, err)
					}

					d.logger.Warn("Failed applying policy to existing dataset", logger.Ctx{"dataset": datasetPath, "err": err})
				}
			}
		} else {
			args := []string{}
			for k, v := range properties {
				args = append(args, fmt.Sprintf("%s=%s", k, v))
			}

			err := d.createDataset(datasetPath, args...)
			if err != nil {
				return fmt.Errorf("Failed creating dataset %q: %w", datasetPath, err)
			}
		}
	}

	return nil
}

// FillConfig populates the storage pool's configuration file with the default values.
func (d *zfs) FillConfig() error {
	vdevType, devices := d.parseSource()
	if !slices.Contains(zfsSupportedVdevTypes, vdevType) {
		return fmt.Errorf("Unsupported ZFS vdev type %q. Supported types are %v", vdevType, zfsSupportedVdevTypes)
	}

	loopPath := loopFilePath(d.name)
	if len(devices) == 1 && !filepath.IsAbs(devices[0]) {
		// Handle an existing zpool.
		if d.config["zfs.pool_name"] == "" {
			d.config["zfs.pool_name"] = devices[0]
		}

		// Unset size property since it's irrelevant.
		d.config["size"] = ""
	} else if len(devices) == 0 || (len(devices) == 1 && devices[0] == loopPath) {
		// Create a loop based pool.
		d.config["source"] = loopPath

		// Set default pool_name.
		if d.config["zfs.pool_name"] == "" {
			d.config["zfs.pool_name"] = d.name
		}

		// Pick a default size of the loop file if not specified.
		if d.config["size"] == "" {
			defaultSize, err := loopFileSizeDefault()
			if err != nil {
				return err
			}

			d.config["size"] = fmt.Sprintf("%dGiB", defaultSize)
		}
	} else if sliceAny(devices, func(device string) bool { return !linux.IsBlockdevPath(device) }) {
		return fmt.Errorf("Custom loop file locations are not supported")
	} else {
		// Set default pool_name.
		if d.config["zfs.pool_name"] == "" {
			d.config["zfs.pool_name"] = d.name
		}

		// Unset size property since it's irrelevant.
		d.config["size"] = ""
	}

	return nil
}

// Create is called during pool creation and is effectively using an empty driver struct.
// WARNING: The Create() function cannot rely on any of the struct attributes being set.
func (d *zfs) Create() error {
	// Store the provided source as we are likely to be mangling it.
	d.config["volatile.initial_source"] = d.config["source"]

	err := d.FillConfig()
	if err != nil {
		return err
	}

	vdevType, devices := d.parseSource()
	loopPath := loopFilePath(d.name)
	if len(devices) == 1 && !filepath.IsAbs(devices[0]) {
		// Validate pool_name.
		if d.config["zfs.pool_name"] != devices[0] {
			return fmt.Errorf("The source must match zfs.pool_name if specified")
		}

		if strings.Contains(d.config["zfs.pool_name"], "/") {
			// Handle a dataset.
			exists, err := d.datasetExists(d.config["zfs.pool_name"])
			if err != nil {
				return err
			}

			if !exists {
				err := d.createDataset(d.config["zfs.pool_name"], "mountpoint=legacy")
				if err != nil {
					return err
				}
			}
		} else {
			// Ensure that the pool is available.
			_, err := d.importPool()
			if err != nil {
				return err
			}
		}

		// Confirm that the existing pool/dataset is all empty.
		datasets, err := d.getDatasets(d.config["zfs.pool_name"], "all")
		if err != nil {
			return err
		}

		if len(datasets) > 0 {
			return fmt.Errorf(`Provided ZFS pool (or dataset) isn't empty, run "sudo zfs list -r %s" to see existing entries`, d.config["zfs.pool_name"])
		}
	} else if len(devices) == 1 && devices[0] == loopPath {
		// Validate pool_name.
		if strings.Contains(d.config["zfs.pool_name"], "/") {
			return fmt.Errorf("zfs.pool_name can't point to a dataset when source isn't set")
		}

		// Create the loop file itself.
		size, err := units.ParseByteSizeString(d.config["size"])
		if err != nil {
			return err
		}

		err = ensureSparseFile(loopPath, size)
		if err != nil {
			return err
		}

		// Create the zpool.
		createArgs := []string{"create", "-m", "none", "-O", "compression=on", d.config["zfs.pool_name"]}
		// "zpool create" doesn't have an explicit type for "stripe" vdev type
		if vdevType != zfsDefaultVdevType {
			createArgs = append(createArgs, vdevType)
		}

		createArgs = append(createArgs, loopPath)
		_, err = subprocess.RunCommand("zpool", createArgs...)
		if err != nil {
			return err
		}

		// Apply auto-trim if supported.
		if zfsTrim {
			_, err := subprocess.RunCommand("zpool", "set", "autotrim=on", d.config["zfs.pool_name"])
			if err != nil {
				return err
			}
		}
	} else {
		// At this moment, we have assurance from FillConfig that all devices are existing block devices
		// Validate pool_name.
		if strings.Contains(d.config["zfs.pool_name"], "/") {
			return fmt.Errorf("zfs.pool_name can't point to a dataset when source isn't set")
		}

		var createArgs []string
		// Wipe if requested.
		if util.IsTrue(d.config["source.wipe"]) {
			for _, device := range devices {
				err := wipeBlockHeaders(device)
				if err != nil {
					return fmt.Errorf("Failed to wipe headers from disk %q: %w", device, err)
				}
			}

			d.config["source.wipe"] = ""
			createArgs = []string{"create", "-f", "-m", "none", "-O", "compression=on", d.config["zfs.pool_name"]}
		} else {
			createArgs = []string{"create", "-m", "none", "-O", "compression=on", d.config["zfs.pool_name"]}
		}

		// Create the zpool.
		// "zpool create" doesn't have an explicit type for "stripe" vdev type
		if vdevType != zfsDefaultVdevType {
			createArgs = append(createArgs, vdevType)
		}

		createArgs = append(createArgs, devices...)
		_, err = subprocess.RunCommand("zpool", createArgs...)
		if err != nil {
			return err
		}

		// Apply auto-trim if supported.
		if zfsTrim {
			_, err := subprocess.RunCommand("zpool", "set", "autotrim=on", d.config["zfs.pool_name"])
			if err != nil {
				return err
			}
		}

		// We don't need to keep the original source path around for import.
		d.config["source"] = d.config["zfs.pool_name"]
	}

	// Setup revert in case of problems
	reverter := revert.New()
	defer reverter.Fail()

	reverter.Add(func() { _ = d.Delete(nil) })

	// Apply our default configuration.
	err = d.ensureInitialDatasets(false)
	if err != nil {
		return err
	}

	reverter.Success()
	return nil
}

// Delete removes the storage pool from the storage device.
func (d *zfs) Delete(op *operations.Operation) error {
	// Check if the dataset/pool is already gone.
	exists, err := d.datasetExists(d.config["zfs.pool_name"])
	if err != nil {
		return err
	}

	if exists {
		// Confirm that nothing's been left behind
		datasets, err := d.getDatasets(d.config["zfs.pool_name"], "all")
		if err != nil {
			return err
		}

		initialDatasets := d.initialDatasets()
		for _, dataset := range datasets {
			dataset = strings.TrimPrefix(dataset, "/")

			if slices.Contains(initialDatasets, dataset) {
				continue
			}

			fields := strings.Split(dataset, "/")
			if len(fields) > 1 {
				return fmt.Errorf("ZFS pool has leftover datasets: %s", dataset)
			}
		}

		// Delete the pool.
		if strings.Contains(d.config["zfs.pool_name"], "/") {
			// Delete the dataset.
			_, err := subprocess.RunCommand("zfs", "destroy", "-r", d.config["zfs.pool_name"])
			if err != nil {
				return err
			}
		} else {
			// Delete the pool.
			_, err := subprocess.RunCommand("zpool", "destroy", d.config["zfs.pool_name"])
			if err != nil {
				return err
			}
		}
	}

	// On delete, wipe everything in the directory.
	err = wipeDirectory(GetPoolMountPath(d.name))
	if err != nil {
		return err
	}

	// Delete any loop file we may have used
	loopPath := loopFilePath(d.name)
	err = os.Remove(loopPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Failed to remove '%s': %w", loopPath, err)
	}

	return nil
}

// Validate checks that all provide keys are supported and that no conflicting or missing configuration is present.
func (d *zfs) Validate(config map[string]string) error {
	rules := map[string]func(value string) error{
		"size":          validate.Optional(validate.IsSize),
		"zfs.pool_name": validate.IsAny,
		"zfs.clone_copy": validate.Optional(func(value string) error {
			if value == "rebase" {
				return nil
			}

			return validate.IsBool(value)
		}),
		"zfs.export": validate.Optional(validate.IsBool),
	}

	return d.validatePool(config, rules, d.commonVolumeRules())
}

// Update applies any driver changes required from a configuration change.
func (d *zfs) Update(changedConfig map[string]string) error {
	_, ok := changedConfig["zfs.pool_name"]
	if ok {
		return fmt.Errorf("zfs.pool_name cannot be modified")
	}

	size, ok := changedConfig["size"]
	if ok {
		// Figure out loop path
		loopPath := loopFilePath(d.name)

		_, devices := d.parseSource()
		if len(devices) != 1 || devices[0] != loopPath {
			return fmt.Errorf("Cannot resize non-loopback pools")
		}

		// Resize loop file
		f, err := os.OpenFile(loopPath, os.O_RDWR, 0o600)
		if err != nil {
			return err
		}

		defer func() { _ = f.Close() }()

		sizeBytes, _ := units.ParseByteSizeString(size)

		err = f.Truncate(sizeBytes)
		if err != nil {
			return err
		}

		_, err = subprocess.RunCommand("zpool", "online", "-e", d.config["zfs.pool_name"], loopPath)
		if err != nil {
			return err
		}
	}

	return nil
}

// importPool the storage pool.
func (d *zfs) importPool() (bool, error) {
	if d.config["zfs.pool_name"] == "" {
		return false, fmt.Errorf("Cannot mount pool as %q is not specified", "zfs.pool_name")
	}

	// Check if already setup.
	exists, err := d.datasetExists(d.config["zfs.pool_name"])
	if err != nil {
		return false, err
	}

	if exists {
		return false, nil
	}

	// Check if the pool exists.
	poolName := strings.Split(d.config["zfs.pool_name"], "/")[0]
	exists, err = d.datasetExists(poolName)
	if err != nil {
		return false, err
	}

	if exists {
		return false, fmt.Errorf("ZFS zpool exists but dataset is missing")
	}

	// Import the pool.
	if filepath.IsAbs(d.config["source"]) {
		disksPath := internalUtil.VarPath("disks")
		_, err := subprocess.RunCommand("zpool", "import", "-f", "-d", disksPath, poolName)
		if err != nil {
			return false, err
		}
	} else {
		_, err := subprocess.RunCommand("zpool", "import", poolName)
		if err != nil {
			return false, err
		}
	}

	// Check that the dataset now exists.
	exists, err = d.datasetExists(d.config["zfs.pool_name"])
	if err != nil {
		return false, err
	}

	if !exists {
		return false, fmt.Errorf("ZFS zpool exists but dataset is missing")
	}

	// We need to explicitly import the keys here so containers can start. This
	// is always needed because even if the admin has set up auto-import of
	// keys on the system, because incus manually imports and exports the pools
	// the keys can get unloaded.
	//
	// We could do "zpool import -l" to request the keys during import, but by
	// doing it separately we know that the key loading specifically failed and
	// not some other operation. If a user has keylocation=prompt configured,
	// this command will fail and the pool will fail to load.
	_, err = subprocess.RunCommand("zfs", "load-key", "-r", d.config["zfs.pool_name"])
	if err != nil {
		_, _ = d.Unmount()
		return false, fmt.Errorf("Failed to load keys for ZFS dataset %q: %w", d.config["zfs.pool_name"], err)
	}

	return true, nil
}

// Mount mounts the storage pool.
func (d *zfs) Mount() (bool, error) {
	// Import the pool if not already imported.
	imported, err := d.importPool()
	if err != nil {
		return false, err
	}

	// Apply our default configuration.
	err = d.ensureInitialDatasets(true)
	if err != nil {
		return false, err
	}

	return imported, nil
}

// Unmount unmounts the storage pool.
func (d *zfs) Unmount() (bool, error) {
	// Skip if zfs.export config is set to false
	if util.IsFalse(d.config["zfs.export"]) {
		return false, nil
	}

	// Skip if using a dataset and not a full pool.
	if strings.Contains(d.config["zfs.pool_name"], "/") {
		return false, nil
	}

	// Check if already unmounted.
	exists, err := d.datasetExists(d.config["zfs.pool_name"])
	if err != nil {
		return false, err
	}

	if !exists {
		return false, nil
	}

	// Export the pool.
	poolName := strings.Split(d.config["zfs.pool_name"], "/")[0]
	_, err = subprocess.RunCommand("zpool", "export", poolName)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (d *zfs) GetResources() (*api.ResourcesStoragePool, error) {
	// Get the total amount of space.
	availableStr, err := d.getDatasetProperty(d.config["zfs.pool_name"], "available")
	if err != nil {
		return nil, err
	}

	available, err := strconv.ParseUint(strings.TrimSpace(availableStr), 10, 64)
	if err != nil {
		return nil, err
	}

	// Get the used amount of space.
	usedStr, err := d.getDatasetProperty(d.config["zfs.pool_name"], "used")
	if err != nil {
		return nil, err
	}

	used, err := strconv.ParseUint(strings.TrimSpace(usedStr), 10, 64)
	if err != nil {
		return nil, err
	}

	// Build the struct.
	// Inode allocation is dynamic so no use in reporting them.
	res := api.ResourcesStoragePool{}
	res.Space.Total = used + available
	res.Space.Used = used

	return &res, nil
}

// MigrationType returns the type of transfer methods to be used when doing migrations between pools in preference order.
func (d *zfs) MigrationTypes(contentType ContentType, refresh bool, copySnapshots bool, clusterMove bool, storageMove bool) []localMigration.Type {
	var rsyncFeatures []string

	// Do not pass compression argument to rsync if the associated
	// config key, that is rsync.compression, is set to false.
	if util.IsFalse(d.Config()["rsync.compression"]) {
		rsyncFeatures = []string{"xattrs", "delete", "bidirectional"}
	} else {
		rsyncFeatures = []string{"xattrs", "delete", "compress", "bidirectional"}
	}

	// Detect ZFS features.
	features := []string{migration.ZFSFeatureMigrationHeader, "compress"}

	if contentType == ContentTypeFS {
		features = append(features, migration.ZFSFeatureZvolFilesystems)
	}

	if IsContentBlock(contentType) {
		return []localMigration.Type{
			{
				FSType:   migration.MigrationFSType_ZFS,
				Features: features,
			},
			{
				FSType:   migration.MigrationFSType_BLOCK_AND_RSYNC,
				Features: rsyncFeatures,
			},
		}
	}

	if refresh && !copySnapshots {
		return []localMigration.Type{
			{
				FSType:   migration.MigrationFSType_RSYNC,
				Features: rsyncFeatures,
			},
		}
	}

	return []localMigration.Type{
		{
			FSType:   migration.MigrationFSType_ZFS,
			Features: features,
		},
		{
			FSType:   migration.MigrationFSType_RSYNC,
			Features: rsyncFeatures,
		},
	}
}

// patchDropBlockVolumeFilesystemExtension removes the filesystem extension (e.g _ext4) from VM image block volumes.
func (d *zfs) patchDropBlockVolumeFilesystemExtension() error {
	poolName, ok := d.config["zfs.pool_name"]
	if !ok {
		poolName = d.name
	}

	out, err := subprocess.RunCommand("zfs", "list", "-H", "-r", "-o", "name", "-t", "volume", fmt.Sprintf("%s/images", poolName))
	if err != nil {
		return fmt.Errorf("Failed listing images: %w", err)
	}

	for _, volume := range strings.Split(out, "\n") {
		fields := strings.SplitN(volume, fmt.Sprintf("%s/images/", poolName), 2)

		if len(fields) != 2 || fields[1] == "" {
			continue
		}

		// Ignore non-block images, and images without filesystem extension
		if !strings.HasSuffix(fields[1], ".block") || !strings.Contains(fields[1], "_") {
			continue
		}

		// Rename zfs dataset. Snapshots will automatically be renamed.
		newName := fmt.Sprintf("%s/images/%s.block", poolName, strings.Split(fields[1], "_")[0])

		_, err = subprocess.RunCommand("zfs", "rename", volume, newName)
		if err != nil {
			return fmt.Errorf("Failed renaming zfs dataset: %w", err)
		}
	}

	return nil
}

// Returns vdev type and block device(s) from source config.
func (d *zfs) parseSource() (string, []string) {
	sourceParts := strings.Split(d.config["source"], "=")
	vdevType := zfsDefaultVdevType
	devices := sourceParts[0]
	if len(sourceParts) > 1 {
		vdevType = sourceParts[0]
		devices = sourceParts[1]
	}

	if len(devices) == 0 {
		return vdevType, make([]string, 0)
	}

	return vdevType, strings.Split(devices, ",")
}

// roundVolumeBlockSizeBytes returns sizeBytes rounded up to the next multiple
// of `vol`'s "zfs.blocksize".
func (d *zfs) roundVolumeBlockSizeBytes(vol Volume, sizeBytes int64) (int64, error) {
	minBlockSize, err := units.ParseByteSizeString(vol.ExpandedConfig("zfs.blocksize"))

	// minBlockSize will be 0 if zfs.blocksize=""
	if minBlockSize <= 0 || err != nil {
		// 16KiB is the default volblocksize
		minBlockSize = 16 * 1024
	}

	return roundAbove(minBlockSize, sizeBytes), nil
}
