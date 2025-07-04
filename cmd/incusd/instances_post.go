package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	petname "github.com/dustinkirkland/golang-petname"
	"github.com/gorilla/websocket"

	internalInstance "github.com/lxc/incus/v6/internal/instance"
	"github.com/lxc/incus/v6/internal/server/backup"
	"github.com/lxc/incus/v6/internal/server/cluster"
	"github.com/lxc/incus/v6/internal/server/db"
	dbCluster "github.com/lxc/incus/v6/internal/server/db/cluster"
	"github.com/lxc/incus/v6/internal/server/db/operationtype"
	deviceConfig "github.com/lxc/incus/v6/internal/server/device/config"
	"github.com/lxc/incus/v6/internal/server/instance"
	"github.com/lxc/incus/v6/internal/server/instance/instancetype"
	"github.com/lxc/incus/v6/internal/server/instance/operationlock"
	"github.com/lxc/incus/v6/internal/server/lifecycle"
	"github.com/lxc/incus/v6/internal/server/operations"
	"github.com/lxc/incus/v6/internal/server/project"
	"github.com/lxc/incus/v6/internal/server/request"
	"github.com/lxc/incus/v6/internal/server/response"
	"github.com/lxc/incus/v6/internal/server/scriptlet"
	"github.com/lxc/incus/v6/internal/server/state"
	storagePools "github.com/lxc/incus/v6/internal/server/storage"
	internalUtil "github.com/lxc/incus/v6/internal/util"
	"github.com/lxc/incus/v6/internal/version"
	"github.com/lxc/incus/v6/shared/api"
	apiScriptlet "github.com/lxc/incus/v6/shared/api/scriptlet"
	"github.com/lxc/incus/v6/shared/archive"
	"github.com/lxc/incus/v6/shared/logger"
	"github.com/lxc/incus/v6/shared/osarch"
	"github.com/lxc/incus/v6/shared/revert"
	"github.com/lxc/incus/v6/shared/util"
)

func ensureDownloadedImageFitWithinBudget(ctx context.Context, s *state.State, r *http.Request, op *operations.Operation, p api.Project, img *api.Image, imgAlias string, source api.InstanceSource, imgType string) (*api.Image, error) {
	var autoUpdate bool
	var err error
	if p.Config["images.auto_update_cached"] != "" {
		autoUpdate = util.IsTrue(p.Config["images.auto_update_cached"])
	} else {
		autoUpdate = s.GlobalConfig.ImagesAutoUpdateCached()
	}

	var budget int64
	err = s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		budget, err = project.GetImageSpaceBudget(tx, p.Name)
		return err
	})
	if err != nil {
		return nil, err
	}

	imgDownloaded, created, err := ImageDownload(ctx, r, s, op, &ImageDownloadArgs{
		Server:       source.Server,
		Protocol:     source.Protocol,
		Certificate:  source.Certificate,
		Secret:       source.Secret,
		Alias:        imgAlias,
		SetCached:    true,
		Type:         imgType,
		AutoUpdate:   autoUpdate,
		Public:       false,
		PreferCached: true,
		ProjectName:  p.Name,
		Budget:       budget,
	})
	if err != nil {
		return nil, err
	}

	if created {
		// Add the image to the authorizer.
		err = s.Authorizer.AddImage(s.ShutdownCtx, p.Name, imgDownloaded.Fingerprint)
		if err != nil {
			logger.Error("Failed to add image to authorizer", logger.Ctx{"fingerprint": imgDownloaded.Fingerprint, "project": p.Name, "error": err})
		}

		s.Events.SendLifecycle(p.Name, lifecycle.ImageCreated.Event(imgDownloaded.Fingerprint, p.Name, op.Requestor(), logger.Ctx{"type": imgDownloaded.Type}))
	}

	return imgDownloaded, nil
}

func createFromImage(s *state.State, r *http.Request, p api.Project, profiles []api.Profile, img *api.Image, imgAlias string, req *api.InstancesPost) response.Response {
	if s.ServerClustered && s.DB.Cluster.LocalNodeIsEvacuated() {
		return response.Forbidden(fmt.Errorf("Cluster member is evacuated"))
	}

	dbType, err := instancetype.New(string(req.Type))
	if err != nil {
		return response.BadRequest(err)
	}

	run := func(op *operations.Operation) error {
		devices := deviceConfig.NewDevices(req.Devices)

		args := db.InstanceArgs{
			Project:     p.Name,
			Config:      req.Config,
			Type:        dbType,
			Description: req.Description,
			Devices:     deviceConfig.ApplyDeviceInitialValues(devices, profiles),
			Ephemeral:   req.Ephemeral,
			Name:        req.Name,
			Profiles:    profiles,
		}

		if req.Source.Server != "" {
			img, err = ensureDownloadedImageFitWithinBudget(context.TODO(), s, r, op, p, img, imgAlias, req.Source, string(req.Type))
			if err != nil {
				return err
			}
		} else if img != nil {
			err := ensureImageIsLocallyAvailable(context.TODO(), s, r, img, args.Project, args.Type)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("Image not provided for instance creation")
		}

		args.Architecture, err = osarch.ArchitectureID(img.Architecture)
		if err != nil {
			return err
		}

		// Actually create the instance.
		err = instanceCreateFromImage(context.TODO(), s, r, img, args, op)
		if err != nil {
			return err
		}

		return instanceCreateFinish(s, req, args, op)
	}

	resources := map[string][]api.URL{}
	resources["instances"] = []api.URL{*api.NewURL().Path(version.APIVersion, "instances", req.Name)}

	op, err := operations.OperationCreate(s, p.Name, operations.OperationClassTask, operationtype.InstanceCreate, resources, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	return operations.OperationResponse(op)
}

func createFromNone(s *state.State, r *http.Request, projectName string, profiles []api.Profile, req *api.InstancesPost) response.Response {
	if s.ServerClustered && s.DB.Cluster.LocalNodeIsEvacuated() {
		return response.Forbidden(fmt.Errorf("Cluster member is evacuated"))
	}

	dbType, err := instancetype.New(string(req.Type))
	if err != nil {
		return response.BadRequest(err)
	}

	devices := deviceConfig.NewDevices(req.Devices)

	args := db.InstanceArgs{
		Project:     projectName,
		Config:      req.Config,
		Type:        dbType,
		Description: req.Description,
		Devices:     deviceConfig.ApplyDeviceInitialValues(devices, profiles),
		Ephemeral:   req.Ephemeral,
		Name:        req.Name,
		Profiles:    profiles,
	}

	if req.Architecture != "" {
		architecture, err := osarch.ArchitectureID(req.Architecture)
		if err != nil {
			return response.InternalError(err)
		}

		args.Architecture = architecture
	}

	run := func(op *operations.Operation) error {
		// Actually create the instance.
		_, err := instanceCreateAsEmpty(s, args, op)
		if err != nil {
			return err
		}

		return instanceCreateFinish(s, req, args, op)
	}

	resources := map[string][]api.URL{}
	resources["instances"] = []api.URL{*api.NewURL().Path(version.APIVersion, "instances", req.Name)}

	op, err := operations.OperationCreate(s, projectName, operations.OperationClassTask, operationtype.InstanceCreate, resources, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	return operations.OperationResponse(op)
}

func createFromMigration(ctx context.Context, s *state.State, r *http.Request, projectName string, profiles []api.Profile, req *api.InstancesPost) response.Response {
	if s.ServerClustered && r != nil && r.Context().Value(request.CtxProtocol) != "cluster" && s.DB.Cluster.LocalNodeIsEvacuated() {
		return response.Forbidden(fmt.Errorf("Cluster member is evacuated"))
	}

	// Validate migration mode.
	if req.Source.Mode != "pull" && req.Source.Mode != "push" {
		return response.NotImplemented(fmt.Errorf("Mode %q not implemented", req.Source.Mode))
	}

	// Parse the architecture name
	architecture, err := osarch.ArchitectureID(req.Architecture)
	if err != nil {
		return response.BadRequest(err)
	}

	dbType, err := instancetype.New(string(req.Type))
	if err != nil {
		return response.BadRequest(err)
	}

	if dbType != instancetype.Container && dbType != instancetype.VM {
		return response.BadRequest(fmt.Errorf("Instance type not supported %q", req.Type))
	}

	// Prepare the instance creation request.
	args := db.InstanceArgs{
		Project:      projectName,
		Architecture: architecture,
		BaseImage:    req.Source.BaseImage,
		Config:       req.Config,
		Type:         dbType,
		Devices:      deviceConfig.NewDevices(req.Devices),
		Description:  req.Description,
		Ephemeral:    req.Ephemeral,
		Name:         req.Name,
		Profiles:     profiles,
		Stateful:     req.Stateful,
	}

	storagePool, storagePoolProfile, localRootDiskDeviceKey, localRootDiskDevice, resp := instanceFindStoragePool(ctx, s, projectName, req)
	if resp != nil {
		return resp
	}

	if storagePool == "" {
		return response.BadRequest(fmt.Errorf("Can't find a storage pool for the instance to use"))
	}

	if localRootDiskDeviceKey == "" && storagePoolProfile == "" {
		// Give the container it's own local root disk device with a pool property.
		rootDev := map[string]string{}
		rootDev["type"] = "disk"
		rootDev["path"] = "/"
		rootDev["pool"] = storagePool
		if args.Devices == nil {
			args.Devices = deviceConfig.Devices{}
		}

		// Make sure that we do not overwrite a device the user is currently using under the
		// name "root".
		rootDevName := "root"
		for i := 0; i < 100; i++ {
			if args.Devices[rootDevName] == nil {
				break
			}

			rootDevName = fmt.Sprintf("root%d", i)
			continue
		}

		args.Devices[rootDevName] = rootDev
	} else if localRootDiskDeviceKey != "" && localRootDiskDevice["pool"] == "" {
		args.Devices[localRootDiskDeviceKey]["pool"] = storagePool
	}

	var inst instance.Instance
	var instOp *operationlock.InstanceOperation
	var cleanup revert.Hook

	// Decide if this is an internal cluster move request.
	var clusterMoveSourceName string
	if r != nil && isClusterNotification(r) && req.Source.Source != "" {
		clusterMoveSourceName = req.Source.Source
	}

	// Early check for refresh and cluster same name move to check instance exists.
	if req.Source.Refresh || (clusterMoveSourceName != "" && clusterMoveSourceName == req.Name) {
		inst, err = instance.LoadByProjectAndName(s, projectName, req.Name)
		if err != nil {
			if response.IsNotFoundError(err) {
				if clusterMoveSourceName != "" {
					// Cluster move doesn't allow renaming as part of migration so fail here.
					return response.SmartError(fmt.Errorf("Cluster move doesn't allow renaming"))
				}

				req.Source.Refresh = false
			} else {
				return response.SmartError(err)
			}
		}
	}

	reverter := revert.New()
	defer reverter.Fail()

	instanceOnly := req.Source.InstanceOnly

	if inst == nil {
		_, err := storagePools.LoadByName(s, storagePool)
		if err != nil {
			return response.InternalError(err)
		}

		// Create the instance DB record for main instance.
		// Note: At this stage we do not yet know if snapshots are going to be received and so we cannot
		// create their DB records. This will be done if needed in the migrationSink.Do() function called
		// as part of the operation below.
		inst, instOp, cleanup, err = instance.CreateInternal(s, args, nil, true, false)
		if err != nil {
			return response.InternalError(fmt.Errorf("Failed creating instance record: %w", err))
		}

		reverter.Add(cleanup)
	} else {
		instOp, err = inst.LockExclusive()
		if err != nil {
			return response.SmartError(fmt.Errorf("Failed getting exclusive access to instance: %w", err))
		}
	}

	reverter.Add(func() { instOp.Done(err) })

	push := false
	var dialer *websocket.Dialer

	if req.Source.Mode == "push" {
		push = true
	} else {
		dialer, err = setupWebsocketDialer(req.Source.Certificate)
		if err != nil {
			return response.SmartError(fmt.Errorf("Failed setting up websocket dialer for migration sink connections: %w", err))
		}
	}

	migrationArgs := migrationSinkArgs{
		URL:                   req.Source.Operation,
		Dialer:                dialer,
		Instance:              inst,
		Secrets:               req.Source.Websockets,
		Push:                  push,
		Live:                  req.Source.Live,
		InstanceOnly:          instanceOnly,
		ClusterMoveSourceName: clusterMoveSourceName,
		Refresh:               req.Source.Refresh,
		RefreshExcludeOlder:   req.Source.RefreshExcludeOlder,
		StoragePool:           storagePool,
	}

	// Check if the pool is changing at all.
	if r != nil && isClusterNotification(r) && inst != nil {
		_, currentPool, _ := internalInstance.GetRootDiskDevice(inst.ExpandedDevices().CloneNative())
		if currentPool["pool"] == storagePool {
			migrationArgs.StoragePool = ""
		}
	}

	sink, err := newMigrationSink(&migrationArgs)
	if err != nil {
		return response.InternalError(err)
	}

	// Copy reverter so far so we can use it inside run after this function has finished.
	runReverter := reverter.Clone()

	run := func(op *operations.Operation) error {
		defer runReverter.Fail()

		sink.instance.SetOperation(op)

		// And finally run the migration.
		err = sink.Do(s, instOp)
		if err != nil {
			err = fmt.Errorf("Error transferring instance data: %w", err)
			instOp.Done(err) // Complete operation that was created earlier, to release lock.

			return err
		}

		instOp.Done(nil) // Complete operation that was created earlier, to release lock.

		if migrationArgs.StoragePool != "" {
			// Update root device for the instance.
			err = s.DB.Cluster.Transaction(context.Background(), func(ctx context.Context, tx *db.ClusterTx) error {
				devs := inst.LocalDevices().CloneNative()
				rootDevKey, _, err := internalInstance.GetRootDiskDevice(inst.ExpandedDevices().CloneNative())
				if err != nil {
					if !errors.Is(err, internalInstance.ErrNoRootDisk) {
						return err
					}

					rootDev := map[string]string{}
					rootDev["type"] = "disk"
					rootDev["path"] = "/"
					rootDev["pool"] = storagePool

					devs["root"] = rootDev
				} else {
					// Copy the device if not a local device.
					_, ok := devs[rootDevKey]
					if !ok {
						devs[rootDevKey] = inst.ExpandedDevices().CloneNative()[rootDevKey]
					}

					// Apply the override.
					devs[rootDevKey]["pool"] = storagePool
				}

				devices, err := dbCluster.APIToDevices(devs)
				if err != nil {
					return err
				}

				id, err := dbCluster.GetInstanceID(ctx, tx.Tx(), inst.Project().Name, inst.Name())
				if err != nil {
					return fmt.Errorf("Failed to get ID of moved instance: %w", err)
				}

				err = dbCluster.UpdateInstanceDevices(ctx, tx.Tx(), int64(id), devices)
				if err != nil {
					return err
				}

				return nil
			})
			if err != nil {
				return err
			}
		}

		runReverter.Success()

		return instanceCreateFinish(s, req, args, op)
	}

	resources := map[string][]api.URL{}
	resources["instances"] = []api.URL{*api.NewURL().Path(version.APIVersion, "instances", req.Name)}

	var op *operations.Operation
	if push {
		op, err = operations.OperationCreate(s, projectName, operations.OperationClassWebsocket, operationtype.InstanceCreate, resources, sink.Metadata(), run, nil, sink.Connect, r)
		if err != nil {
			return response.InternalError(err)
		}
	} else {
		op, err = operations.OperationCreate(s, projectName, operations.OperationClassTask, operationtype.InstanceCreate, resources, nil, run, nil, nil, r)
		if err != nil {
			return response.InternalError(err)
		}
	}

	reverter.Success()
	return operations.OperationResponse(op)
}

func createFromCopy(ctx context.Context, s *state.State, r *http.Request, projectName string, profiles []api.Profile, req *api.InstancesPost) response.Response {
	if s.ServerClustered && s.DB.Cluster.LocalNodeIsEvacuated() {
		return response.Forbidden(fmt.Errorf("Cluster member is evacuated"))
	}

	if req.Source.Source == "" {
		return response.BadRequest(fmt.Errorf("Must specify a source instance"))
	}

	sourceProject := req.Source.Project
	if sourceProject == "" {
		sourceProject = projectName
	}

	targetProject := projectName

	source, err := instance.LoadByProjectAndName(s, sourceProject, req.Source.Source)
	if err != nil {
		return response.SmartError(err)
	}

	// When clustered, use the node name, otherwise use the hostname.
	if s.ServerClustered {
		serverName := s.ServerName

		if serverName != source.Location() {
			// Check if we are copying from a remote storage instance.
			_, rootDevice, _ := internalInstance.GetRootDiskDevice(source.ExpandedDevices().CloneNative())
			sourcePoolName := rootDevice["pool"]

			destPoolName, _, _, _, resp := instanceFindStoragePool(r.Context(), s, targetProject, req)
			if resp != nil {
				return resp
			}

			if sourcePoolName != destPoolName {
				// Redirect to migration
				return clusterCopyContainerInternal(ctx, s, r, source, projectName, profiles, req)
			}

			var pool *api.StoragePool

			err = s.DB.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
				_, pool, _, err = tx.GetStoragePoolInAnyState(ctx, sourcePoolName)

				return err
			})
			if err != nil {
				err = fmt.Errorf("Failed to fetch instance's pool info: %w", err)
				return response.SmartError(err)
			}

			if !slices.Contains(db.StorageRemoteDriverNames(), pool.Driver) {
				// Redirect to migration
				return clusterCopyContainerInternal(ctx, s, r, source, projectName, profiles, req)
			}
		}
	}

	// Config override
	sourceConfig := source.LocalConfig()
	if req.Config == nil {
		req.Config = make(map[string]string)
	}

	for key, value := range sourceConfig {
		if !internalInstance.InstanceIncludeWhenCopying(key, false) {
			logger.Debug("Skipping key from copy source", logger.Ctx{"key": key, "sourceProject": source.Project().Name, "sourceInstance": source.Name(), "project": targetProject, "instance": req.Name})
			continue
		}

		_, exists := req.Config[key]
		if exists {
			continue
		}

		req.Config[key] = value
	}

	// Devices override
	sourceDevices := source.LocalDevices()

	if req.Devices == nil {
		req.Devices = make(map[string]map[string]string)
	}

	for key, value := range sourceDevices {
		_, exists := req.Devices[key]
		if exists {
			continue
		}

		req.Devices[key] = value
	}

	if req.Stateful {
		sourceName, _, _ := api.GetParentAndSnapshotName(source.Name())
		if sourceName != req.Name {
			return response.BadRequest(fmt.Errorf("Instance name cannot be changed during stateful copy (%q to %q)", sourceName, req.Name))
		}
	}

	dbType, err := instancetype.New(string(req.Type))
	if err != nil {
		return response.BadRequest(err)
	}

	// If type isn't specified, match the source type.
	if req.Type == "" {
		dbType = source.Type()
	}

	if dbType != instancetype.Any && dbType != source.Type() {
		return response.BadRequest(fmt.Errorf("Instance type should not be specified or should match source type"))
	}

	args := db.InstanceArgs{
		Project:      targetProject,
		Architecture: source.Architecture(),
		BaseImage:    req.Source.BaseImage,
		Config:       req.Config,
		Type:         source.Type(),
		Description:  req.Description,
		Devices:      deviceConfig.NewDevices(req.Devices),
		Ephemeral:    req.Ephemeral,
		Name:         req.Name,
		Profiles:     profiles,
		Stateful:     req.Stateful,
	}

	run := func(op *operations.Operation) error {
		// Actually create the instance.
		_, err := instanceCreateAsCopy(s, instanceCreateAsCopyOpts{
			sourceInstance:       source,
			targetInstance:       args,
			instanceOnly:         req.Source.InstanceOnly,
			refresh:              req.Source.Refresh,
			refreshExcludeOlder:  req.Source.RefreshExcludeOlder,
			applyTemplateTrigger: true,
			allowInconsistent:    req.Source.AllowInconsistent,
		}, op)
		if err != nil {
			return err
		}

		return instanceCreateFinish(s, req, args, op)
	}

	resources := map[string][]api.URL{}
	resources["instances"] = []api.URL{*api.NewURL().Path(version.APIVersion, "instances", req.Name), *api.NewURL().Path(version.APIVersion, "instances", req.Source.Source)}

	op, err := operations.OperationCreate(s, targetProject, operations.OperationClassTask, operationtype.InstanceCreate, resources, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	return operations.OperationResponse(op)
}

func createFromBackup(s *state.State, r *http.Request, projectName string, data io.Reader, pool string, instanceName string) response.Response {
	reverter := revert.New()
	defer reverter.Fail()

	// Create temporary file to store uploaded backup data.
	backupFile, err := os.CreateTemp(internalUtil.VarPath("backups"), fmt.Sprintf("%s_", backup.WorkingDirPrefix))
	if err != nil {
		return response.InternalError(err)
	}

	defer func() { _ = os.Remove(backupFile.Name()) }()
	reverter.Add(func() { _ = backupFile.Close() })

	// Stream uploaded backup data into temporary file.
	_, err = io.Copy(backupFile, data)
	if err != nil {
		return response.InternalError(err)
	}

	// Detect squashfs compression and convert to tarball.
	_, err = backupFile.Seek(0, io.SeekStart)
	if err != nil {
		return response.InternalError(err)
	}

	_, algo, decomArgs, err := archive.DetectCompressionFile(backupFile)
	if err != nil {
		return response.InternalError(err)
	}

	if algo == ".squashfs" {
		// Pass the temporary file as program argument to the decompression command.
		decomArgs := append(decomArgs, backupFile.Name())

		// Create temporary file to store the decompressed tarball in.
		tarFile, err := os.CreateTemp(internalUtil.VarPath("backups"), fmt.Sprintf("%s_decompress_", backup.WorkingDirPrefix))
		if err != nil {
			return response.InternalError(err)
		}

		defer func() { _ = os.Remove(tarFile.Name()) }()

		// Decompress to tarFile temporary file.
		err = archive.ExtractWithFds(decomArgs[0], decomArgs[1:], nil, nil, tarFile)
		if err != nil {
			return response.InternalError(err)
		}

		// We don't need the original squashfs file anymore.
		_ = backupFile.Close()
		_ = os.Remove(backupFile.Name())

		// Replace the backup file handle with the handle to the tar file.
		backupFile = tarFile
	}

	// Parse the backup information.
	_, err = backupFile.Seek(0, io.SeekStart)
	if err != nil {
		return response.InternalError(err)
	}

	bInfo, err := backup.GetInfo(backupFile, s.OS, backupFile.Name())
	if err != nil {
		return response.BadRequest(err)
	}

	// Detect broken legacy backups.
	if bInfo.Config == nil {
		return response.BadRequest(fmt.Errorf("Backup file is missing required information"))
	}

	// Check project permissions.
	var req api.InstancesPost
	err = s.DB.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
		req = api.InstancesPost{
			InstancePut: bInfo.Config.Container.InstancePut,
			Name:        bInfo.Name,
			Source:      api.InstanceSource{}, // Only relevant for "copy" or "migration", but may not be nil.
			Type:        api.InstanceType(bInfo.Config.Container.Type),
		}

		return project.AllowInstanceCreation(tx, projectName, req)
	})
	if err != nil {
		return response.SmartError(err)
	}

	bInfo.Project = projectName

	// Override pool.
	if pool != "" {
		bInfo.Pool = pool
	}

	// Override instance name.
	if instanceName != "" {
		bInfo.Name = instanceName
	}

	logger.Debug("Backup file info loaded", logger.Ctx{
		"type":      bInfo.Type,
		"name":      bInfo.Name,
		"project":   bInfo.Project,
		"backend":   bInfo.Backend,
		"pool":      bInfo.Pool,
		"optimized": *bInfo.OptimizedStorage,
		"snapshots": bInfo.Snapshots,
	})

	err = s.DB.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
		// Check storage pool exists.
		_, _, _, err = tx.GetStoragePoolInAnyState(ctx, bInfo.Pool)

		return err
	})
	if response.IsNotFoundError(err) {
		// The storage pool doesn't exist. If backup is in binary format (so we cannot alter
		// the backup.yaml) or the pool has been specified directly from the user restoring
		// the backup then we cannot proceed so return an error.
		if *bInfo.OptimizedStorage || pool != "" {
			return response.InternalError(fmt.Errorf("Storage pool not found: %w", err))
		}

		var profile *api.Profile

		err = s.DB.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
			// Otherwise try and restore to the project's default profile pool.
			_, profile, err = tx.GetProfile(ctx, bInfo.Project, "default")

			return err
		})
		if err != nil {
			return response.InternalError(fmt.Errorf("Failed to get default profile: %w", err))
		}

		_, v, err := internalInstance.GetRootDiskDevice(profile.Devices)
		if err != nil {
			return response.InternalError(fmt.Errorf("Failed to get root disk device: %w", err))
		}

		// Use the default-profile's root pool.
		bInfo.Pool = v["pool"]
	} else if err != nil {
		return response.InternalError(err)
	}

	// Copy reverter so far so we can use it inside run after this function has finished.
	runReverter := reverter.Clone()

	run := func(op *operations.Operation) error {
		defer func() { _ = backupFile.Close() }()
		defer runReverter.Fail()

		pool, err := storagePools.LoadByName(s, bInfo.Pool)
		if err != nil {
			return err
		}

		// Check if the backup is optimized that the source pool driver matches the target pool driver.
		if *bInfo.OptimizedStorage && pool.Driver().Info().Name != bInfo.Backend {
			return fmt.Errorf("Optimized backup storage driver %q differs from the target storage pool driver %q", bInfo.Backend, pool.Driver().Info().Name)
		}

		// Dump tarball to storage. Because the backup file is unpacked and restored onto the storage
		// device before the instance is created in the database it is necessary to return two functions;
		// a post hook that can be run once the instance has been created in the database to run any
		// storage layer finalisations, and a revert hook that can be run if the instance database load
		// process fails that will remove anything created thus far.
		postHook, revertHook, err := pool.CreateInstanceFromBackup(*bInfo, backupFile, nil)
		if err != nil {
			return fmt.Errorf("Create instance from backup: %w", err)
		}

		runReverter.Add(revertHook)

		err = internalImportFromBackup(context.TODO(), s, bInfo.Project, bInfo.Name, instanceName != "")
		if err != nil {
			return fmt.Errorf("Failed importing backup: %w", err)
		}

		inst, err := instance.LoadByProjectAndName(s, bInfo.Project, bInfo.Name)
		if err != nil {
			return fmt.Errorf("Failed loading instance: %w", err)
		}

		// Clean up created instance if the post hook fails below.
		runReverter.Add(func() { _ = inst.Delete(true) })

		// Run the storage post hook to perform any final actions now that the instance has been created
		// in the database (this normally includes unmounting volumes that were mounted).
		if postHook != nil {
			err = postHook(inst)
			if err != nil {
				return fmt.Errorf("Post hook failed: %w", err)
			}
		}

		runReverter.Success()

		return instanceCreateFinish(s, &req, db.InstanceArgs{Name: bInfo.Name, Project: bInfo.Project}, op)
	}

	resources := map[string][]api.URL{}
	resources["instances"] = []api.URL{*api.NewURL().Path(version.APIVersion, "instances", bInfo.Name)}

	op, err := operations.OperationCreate(s, bInfo.Project, operations.OperationClassTask, operationtype.BackupRestore, resources, nil, run, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	reverter.Success()
	return operations.OperationResponse(op)
}

// swagger:operation POST /1.0/instances instances instances_post
//
//	Create a new instance
//
//	Creates a new instance.
//	Depending on the source, this can create an instance from an existing
//	local image, remote image, existing local instance or snapshot, remote
//	migration stream or backup file.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: query
//	    name: project
//	    description: Project name
//	    type: string
//	    example: default
//	  - in: query
//	    name: target
//	    description: Cluster member
//	    type: string
//	    example: default
//	  - in: body
//	    name: instance
//	    description: Instance request
//	    required: false
//	    schema:
//	      $ref: "#/definitions/InstancesPost"
//	  - in: body
//	    name: raw_backup
//	    description: Raw backup file
//	    required: false
//	responses:
//	  "202":
//	    $ref: "#/responses/Operation"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func instancesPost(d *Daemon, r *http.Request) response.Response {
	s := d.State()

	targetProjectName := request.ProjectParam(r)
	clusterNotification := isClusterNotification(r)
	clusterInternal := isClusterInternal(r)

	logger.Debug("Responding to instance create")

	// If we're getting binary content, process separately
	if r.Header.Get("Content-Type") == "application/octet-stream" {
		return createFromBackup(s, r, targetProjectName, r.Body, r.Header.Get("X-Incus-pool"), r.Header.Get("X-Incus-name"))
	}

	// Parse the request
	req := api.InstancesPost{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Set type from URL if missing
	if req.Type == "" {
		req.Type = api.InstanceTypeContainer // Default to container if not specified.
	}

	if req.Devices == nil {
		req.Devices = map[string]map[string]string{}
	}

	if req.Config == nil {
		req.Config = map[string]string{}
	}

	if req.InstanceType != "" {
		conf, err := instanceParseType(req.InstanceType)
		if err != nil {
			return response.BadRequest(err)
		}

		for k, v := range conf {
			if req.Config[k] == "" {
				req.Config[k] = v
			}
		}
	}

	// Special handling for instance refresh.
	// For all other situations, we're headed towards the scheduler, but for this case, we can short circuit it.
	if s.ServerClustered && !clusterNotification && req.Source.Type == "migration" && req.Source.Refresh {
		client, err := cluster.ConnectIfInstanceIsRemote(s, targetProjectName, req.Name, r)
		if err != nil && !response.IsNotFoundError(err) {
			return response.SmartError(err)
		}

		if client != nil {
			// The request needs to be forwarded to the correct server.
			op, err := client.CreateInstance(req)
			if err != nil {
				return response.SmartError(err)
			}

			opAPI := op.Get()
			return operations.ForwardedOperationResponse(targetProjectName, &opAPI)
		}

		if err == nil {
			// The instance is valid and the request wasn't forwarded, so the instance is local.
			return createFromMigration(r.Context(), s, r, targetProjectName, nil, &req)
		}
	}

	var targetProject *api.Project
	var profiles []api.Profile
	var sourceInst *dbCluster.Instance
	var sourceImage *api.Image
	var sourceImageRef string
	var candidateMembers []db.NodeInfo
	var targetMemberInfo *db.NodeInfo
	var targetGroupName string

	target := request.QueryParam(r, "target")
	if !s.ServerClustered && target != "" {
		return response.BadRequest(fmt.Errorf("Target only allowed when clustered"))
	}

	err = s.DB.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
		dbProject, err := dbCluster.GetProject(ctx, tx.Tx(), targetProjectName)
		if err != nil {
			return fmt.Errorf("Failed loading project: %w", err)
		}

		targetProject, err = dbProject.ToAPI(ctx, tx.Tx())
		if err != nil {
			return err
		}

		var allMembers []db.NodeInfo

		if s.ServerClustered && !clusterNotification {
			allMembers, err = tx.GetNodes(ctx)
			if err != nil {
				return fmt.Errorf("Failed getting cluster members: %w", err)
			}

			// Check if the given target is allowed and try to resolve the right member or group
			targetMemberInfo, targetGroupName, err = project.CheckTarget(ctx, s.Authorizer, r, tx, targetProject, target, allMembers)
			if err != nil {
				return err
			}
		}

		profileProject := project.ProfileProjectFromRecord(targetProject)

		switch req.Source.Type {
		case "copy":
			if req.Source.Source == "" {
				return api.StatusErrorf(http.StatusBadRequest, "Must specify a source instance")
			}

			if req.Source.Project == "" {
				req.Source.Project = targetProjectName
			}

			sourceInst, err = instance.LoadInstanceDatabaseObject(ctx, tx, req.Source.Project, req.Source.Source)
			if err != nil {
				return err
			}

			req.Type = api.InstanceType(sourceInst.Type.String())

			// Use source instance's profiles if no profile override.
			if req.Profiles == nil {
				sourceInstArgs, err := tx.InstancesToInstanceArgs(ctx, true, *sourceInst)
				if err != nil {
					return err
				}

				req.Profiles = make([]string, 0, len(sourceInstArgs[sourceInst.ID].Profiles))
				for _, profile := range sourceInstArgs[sourceInst.ID].Profiles {
					req.Profiles = append(req.Profiles, profile.Name)
				}
			}

		case "image":
			// Check if the image has an entry in the database but fail only if the error
			// is different than the image not being found.
			sourceImage, err = getSourceImageFromInstanceSource(ctx, s, tx, targetProject.Name, req.Source, &sourceImageRef, string(req.Type))
			if err != nil && !api.StatusErrorCheck(err, http.StatusNotFound) {
				return err
			}

			// If image has an entry in the database then use its profiles if no override provided.
			if sourceImage != nil && req.Profiles == nil {
				req.Architecture = sourceImage.Architecture
				req.Profiles = sourceImage.Profiles
			}
		}

		// Use default profile if no profile list specified (not even an empty list).
		// This mirrors the logic in instance.CreateInternal() that would occur anyway.
		if req.Profiles == nil {
			req.Profiles = []string{"default"}
		}

		// Initialize the profile info list (even if an empty list is provided so this isn't left as nil).
		// This way instances can still be created without any profiles by providing a non-nil empty list.
		profiles = make([]api.Profile, 0, len(req.Profiles))

		// Load profiles.
		if len(req.Profiles) > 0 {
			profileFilters := make([]dbCluster.ProfileFilter, 0, len(req.Profiles))
			for _, profileName := range req.Profiles {
				profileName := profileName
				profileFilters = append(profileFilters, dbCluster.ProfileFilter{
					Project: &profileProject,
					Name:    &profileName,
				})
			}

			dbProfiles, err := dbCluster.GetProfiles(ctx, tx.Tx(), profileFilters...)
			if err != nil {
				return err
			}

			dbProfileConfigs, err := dbCluster.GetAllProfileConfigs(ctx, tx.Tx())
			if err != nil {
				return err
			}

			dbProfileDevices, err := dbCluster.GetAllProfileDevices(ctx, tx.Tx())
			if err != nil {
				return err
			}

			profilesByName := make(map[string]dbCluster.Profile, len(dbProfiles))
			for _, dbProfile := range dbProfiles {
				profilesByName[dbProfile.Name] = dbProfile
			}

			for _, profileName := range req.Profiles {
				profile, found := profilesByName[profileName]
				if !found {
					return fmt.Errorf("Requested profile %q doesn't exist", profileName)
				}

				apiProfile, err := profile.ToAPI(ctx, tx.Tx(), dbProfileConfigs, dbProfileDevices)
				if err != nil {
					return err
				}

				profiles = append(profiles, *apiProfile)
			}
		}

		// Generate automatic instance name if not specified.
		if req.Name == "" {
			names, err := tx.GetInstanceNames(ctx, targetProjectName)
			if err != nil {
				return err
			}

			i := 0
			for {
				i++
				req.Name = strings.ToLower(petname.Generate(2, "-"))
				if !slices.Contains(names, req.Name) {
					break
				}

				if i > 100 {
					return fmt.Errorf("Couldn't generate a new unique name after 100 tries")
				}
			}

			logger.Debug("No name provided for new instance, using auto-generated name", logger.Ctx{"project": targetProjectName, "instance": req.Name})
		}

		if s.ServerClustered && !clusterNotification && targetMemberInfo == nil {
			architectures, err := instance.SuitableArchitectures(ctx, s, tx, targetProjectName, sourceInst, sourceImageRef, req)
			if err != nil {
				return err
			}

			// If no architectures have been ascertained from the source then use the default
			// architecture from project or global config if available.
			if len(architectures) < 1 {
				defaultArch := targetProject.Config["images.default_architecture"]
				if defaultArch == "" {
					defaultArch = s.GlobalConfig.ImagesDefaultArchitecture()
				}

				if defaultArch != "" {
					defaultArchID, err := osarch.ArchitectureID(defaultArch)
					if err != nil {
						return err
					}

					architectures = append(architectures, defaultArchID)
				} else {
					architectures = nil // Don't exclude candidate members based on architecture.
				}
			}

			clusterGroupsAllowed := project.GetRestrictedClusterGroups(targetProject)

			candidateMembers, err = tx.GetCandidateMembers(ctx, allMembers, architectures, targetGroupName, clusterGroupsAllowed, s.GlobalConfig.OfflineThreshold())
			if err != nil {
				return err
			}
		}

		if !clusterNotification {
			// Check that the project's limits are not violated. Note this check is performed after
			// automatically generated config values (such as ones from an InstanceType) have been set.
			err = project.AllowInstanceCreation(tx, targetProjectName, req)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	err = instance.ValidName(req.Name, false)
	if err != nil {
		return response.BadRequest(err)
	}

	if s.ServerClustered && !clusterNotification && !clusterInternal {
		// If a target was specified, limit the list of candidates to that target.
		if targetMemberInfo != nil {
			candidateMembers = []db.NodeInfo{*targetMemberInfo}
		}

		// Run instance placement scriptlet if enabled.
		if s.GlobalConfig.InstancesPlacementScriptlet() != "" {
			leaderAddress, err := s.Cluster.LeaderAddress()
			if err != nil {
				return response.InternalError(err)
			}

			// Copy request so we don't modify it when expanding the config.
			reqExpanded := apiScriptlet.InstancePlacement{
				InstancesPost: req,
				Project:       targetProjectName,
				Reason:        apiScriptlet.InstancePlacementReasonNew,
			}

			reqExpanded.Config = db.ExpandInstanceConfig(reqExpanded.Config, profiles)
			reqExpanded.Devices = db.ExpandInstanceDevices(deviceConfig.NewDevices(reqExpanded.Devices), profiles).CloneNative()

			targetMemberInfo, err = scriptlet.InstancePlacementRun(r.Context(), logger.Log, s, &reqExpanded, candidateMembers, leaderAddress)
			if err != nil {
				return response.SmartError(fmt.Errorf("Failed instance placement scriptlet: %w", err))
			}
		}

		// If no target member was selected yet, pick the member with the least number of instances.
		if targetMemberInfo == nil && len(candidateMembers) > 0 {
			targetMemberInfo = &candidateMembers[0]
		}

		if targetMemberInfo == nil {
			return response.InternalError(fmt.Errorf("Couldn't find a cluster member for the instance"))
		}
	}

	// Record the cluster group as a volatile config key if present.
	if !clusterNotification && !clusterInternal && targetGroupName != "" {
		req.Config["volatile.cluster.group"] = targetGroupName
	}

	if targetMemberInfo != nil && targetMemberInfo.Address != "" && targetMemberInfo.Name != s.ServerName {
		client, err := cluster.Connect(targetMemberInfo.Address, s.Endpoints.NetworkCert(), s.ServerCert(), r, true)
		if err != nil {
			return response.SmartError(err)
		}

		client = client.UseProject(targetProjectName)
		client = client.UseTarget(targetMemberInfo.Name)

		logger.Debug("Forward instance post request", logger.Ctx{"local": s.ServerName, "target": targetMemberInfo.Name, "targetAddress": targetMemberInfo.Address})
		op, err := client.CreateInstance(req)
		if err != nil {
			return response.SmartError(err)
		}

		opAPI := op.Get()
		return operations.ForwardedOperationResponse(targetProjectName, &opAPI)
	}

	switch req.Source.Type {
	case "image":
		return createFromImage(s, r, *targetProject, profiles, sourceImage, sourceImageRef, &req)
	case "none":
		return createFromNone(s, r, targetProjectName, profiles, &req)
	case "migration":
		return createFromMigration(r.Context(), s, r, targetProjectName, profiles, &req)
	case "copy":
		return createFromCopy(r.Context(), s, r, targetProjectName, profiles, &req)
	default:
		return response.BadRequest(fmt.Errorf("Unknown source type %s", req.Source.Type))
	}
}

func instanceFindStoragePool(ctx context.Context, s *state.State, projectName string, req *api.InstancesPost) (string, string, string, map[string]string, response.Response) {
	// Grab the container's root device if one is specified
	storagePool := ""
	storagePoolProfile := ""

	localRootDiskDeviceKey, localRootDiskDevice, _ := internalInstance.GetRootDiskDevice(req.Devices)
	if localRootDiskDeviceKey != "" {
		storagePool = localRootDiskDevice["pool"]
	}

	// Handle copying/moving between two storage-api instances.
	if storagePool != "" {
		err := s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
			_, err := tx.GetStoragePoolID(ctx, storagePool)

			return err
		})
		if response.IsNotFoundError(err) {
			storagePool = ""
			// Unset the local root disk device storage pool if not
			// found.
			localRootDiskDevice["pool"] = ""
		}
	}

	// If we don't have a valid pool yet, look through profiles
	if storagePool == "" {
		err := s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
			for _, pName := range req.Profiles {
				_, p, err := tx.GetProfile(ctx, projectName, pName)
				if err != nil {
					return err
				}

				k, v, _ := internalInstance.GetRootDiskDevice(p.Devices)
				if k != "" && v["pool"] != "" {
					// Keep going as we want the last one in the profile chain
					storagePool = v["pool"]
					storagePoolProfile = pName
				}
			}

			return nil
		})
		if err != nil {
			return "", "", "", nil, response.SmartError(err)
		}
	}

	// If there is just a single pool in the database, use that
	if storagePool == "" {
		logger.Debug("No valid storage pool in the container's local root disk device and profiles found")

		var pools []string

		err := s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
			var err error

			pools, err = tx.GetStoragePoolNames(ctx)

			return err
		})
		if err != nil {
			if response.IsNotFoundError(err) {
				return "", "", "", nil, response.BadRequest(fmt.Errorf("This instance does not have any storage pools configured"))
			}

			return "", "", "", nil, response.SmartError(err)
		}

		if len(pools) == 1 {
			storagePool = pools[0]
		}
	}

	return storagePool, storagePoolProfile, localRootDiskDeviceKey, localRootDiskDevice, nil
}

func clusterCopyContainerInternal(ctx context.Context, s *state.State, r *http.Request, source instance.Instance, projectName string, profiles []api.Profile, req *api.InstancesPost) response.Response {
	// Locate the source of the container
	var nodeAddress string
	err := s.DB.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		// Load source node.
		nodeAddress, err = tx.GetNodeAddressOfInstance(ctx, source.Project().Name, source.Name())
		if err != nil {
			return fmt.Errorf("Failed to get address of instance's member: %w", err)
		}

		return nil
	})
	if err != nil {
		return response.SmartError(err)
	}

	if nodeAddress == "" {
		return response.BadRequest(fmt.Errorf("The source instance is currently offline"))
	}

	// Connect to the container source
	client, err := cluster.Connect(nodeAddress, s.Endpoints.NetworkCert(), s.ServerCert(), r, false)
	if err != nil {
		return response.SmartError(err)
	}

	client = client.UseProject(source.Project().Name)

	// Setup websockets
	var opAPI api.Operation
	if internalInstance.IsSnapshot(req.Source.Source) {
		cName, sName, _ := api.GetParentAndSnapshotName(req.Source.Source)

		pullReq := api.InstanceSnapshotPost{
			Migration: true,
			Live:      req.Source.Live,
		}

		op, err := client.MigrateInstanceSnapshot(cName, sName, pullReq)
		if err != nil {
			return response.SmartError(err)
		}

		opAPI = op.Get()
	} else {
		instanceOnly := req.Source.InstanceOnly
		pullReq := api.InstancePost{
			Migration:    true,
			Live:         req.Source.Live,
			InstanceOnly: instanceOnly,
		}

		op, err := client.MigrateInstance(req.Source.Source, pullReq)
		if err != nil {
			return response.SmartError(err)
		}

		opAPI = op.Get()
	}

	websockets := map[string]string{}
	for k, v := range opAPI.Metadata {
		websockets[k] = v.(string)
	}

	// Reset the source for a migration
	req.Source.Type = "migration"
	req.Source.Certificate = string(s.Endpoints.NetworkCert().PublicKey())
	req.Source.Mode = "pull"
	req.Source.Operation = fmt.Sprintf("https://%s/%s/operations/%s", nodeAddress, version.APIVersion, opAPI.ID)
	req.Source.Websockets = websockets
	req.Source.Source = ""
	req.Source.Project = ""

	// Run the migration
	return createFromMigration(ctx, s, nil, projectName, profiles, req)
}

func instanceCreateFinish(s *state.State, req *api.InstancesPost, args db.InstanceArgs, op *operations.Operation) error {
	if req == nil || !req.Start {
		return nil
	}

	// Start the instance.
	inst, err := instance.LoadByProjectAndName(s, args.Project, args.Name)
	if err != nil {
		return fmt.Errorf("Failed to load the instance: %w", err)
	}

	inst.SetOperation(op)

	return inst.Start(false)
}
