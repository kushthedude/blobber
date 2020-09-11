package handler

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"

	"net/http"
	"path/filepath"
	"strconv"

	"0chain.net/blobbercore/allocation"
	"0chain.net/blobbercore/config"
	"0chain.net/blobbercore/constants"
	"0chain.net/blobbercore/datastore"
	"0chain.net/blobbercore/filestore"
	"0chain.net/blobbercore/readmarker"
	"0chain.net/blobbercore/reference"
	"0chain.net/blobbercore/stats"
	"0chain.net/blobbercore/writemarker"

	"0chain.net/core/common"
	"0chain.net/core/encryption"
	"0chain.net/core/lock"
	"0chain.net/core/node"

	"gorm.io/gorm"

	. "0chain.net/core/logging"
	"go.uber.org/zap"
)

func readPreRedeem(ctx context.Context, alloc *allocation.Allocation,
	numBlocks, pendNumBlocks int64, clientID string) (err error) {

	if numBlocks == 0 {
		return
	}

	// check out read pool tokens if read_price > 0
	var (
		db        = datastore.GetStore().GetTransaction(ctx)
		blobberID = node.Self.ID
		until     = common.Now() +
			common.Timestamp(config.Configuration.ReadLockTimeout)

		want = alloc.WantRead(blobberID, numBlocks)

		rps []*allocation.ReadPool
	)

	if want == 0 {
		return // skip if read price is zero
	}

	rps, err = allocation.ReadPools(db, clientID, alloc.ID,
		blobberID, until)
	if err != nil {
		return common.NewErrorf("read_pre_redeem",
			"can't get read pools from DB: %v", err)
	}

	var have = alloc.HaveRead(rps, blobberID, pendNumBlocks)

	if have < want {
		rps, err = allocation.RequestReadPools(clientID,
			alloc.ID)
		if err != nil {
			return common.NewErrorf("read_pre_redeem",
				"can't request read pools from sharders: %v", err)
		}

		err = allocation.SetReadPools(db, clientID,
			alloc.ID, blobberID, rps)
		if err != nil {
			return common.NewErrorf("read_pre_redeem",
				"can't save requested read pools: %v", err)
		}

		rps, err = allocation.ReadPools(db, clientID, alloc.ID, blobberID,
			until)
		if err != nil {
			return common.NewErrorf("read_pre_redeem",
				"can't get read pools from DB: %v", err)
		}

		have = alloc.HaveRead(rps, blobberID, pendNumBlocks)
	}

	if have < want {
		return common.NewError("read_pre_redeem", "not enough "+
			"tokens in client's read pools associated with the"+
			" allocation->blobber")
	}

	return
}

func writePreRedeem(ctx context.Context, alloc *allocation.Allocation,
	writeMarker *writemarker.WriteMarker) (err error) {

	// check out read pool tokens if read_price > 0
	var (
		db        = datastore.GetStore().GetTransaction(ctx)
		blobberID = node.Self.ID
		until     = common.Now() +
			common.Timestamp(config.Configuration.WriteLockTimeout)

		want = alloc.WantWrite(blobberID, writeMarker.Size)

		pend *allocation.Pending
		wps  []*allocation.WritePool
	)

	if writeMarker.Size <= 0 || want <= 0 {
		return // skip if write price is zero or it's about deleting
	}

	pend, err = allocation.GetPending(db, writeMarker.ClientID,
		alloc.ID, blobberID)
	if err != nil {
		return common.NewErrorf("write_pre_redeem",
			"can't get pending payments: %v", err)
	}

	wps, err = pend.WritePools(db, blobberID, until)
	if err != nil {
		return common.NewErrorf("write_pre_redeem",
			"can't get read pools from DB: %v", err)
	}

	var have = pend.HaveWrite(wps, alloc)
	if have < want {
		wps, err = allocation.RequestWritePools(writeMarker.ClientID,
			alloc.ID)
		if err != nil {
			return common.NewErrorf("write_pre_redeem",
				"can't request write pools from sharders: %v", err)
		}
		err = allocation.SetWritePools(db, writeMarker.ClientID,
			alloc.ID, blobberID, wps)
		if err != nil {
			return common.NewErrorf("write_pre_redeem",
				"can't save requested write pools: %v", err)
		}
		wps, err = pend.WritePools(db, blobberID, until)
		if err != nil {
			return common.NewErrorf("write_pre_redeem",
				"can't get write pools from DB: %v", err)
		}
		have = pend.HaveWrite(wps, alloc)
	}

	if have < want {
		return common.NewErrorf("write_pre_redeem", "not enough "+
			"tokens in write pools (client -> allocation ->  blobber)"+
			"(%s -> %s -> %s), have %d, want %d", writeMarker.ClientID,
			alloc.ID, writeMarker.BlobberID, have, want)
	}

	// update pending writes: add size to redeem to (not tokens)
	pend.AddPendingWrite(writeMarker.Size)
	if err = pend.Save(db); err != nil {
		return common.NewErrorf("write_pre_redeem",
			"can't save pending writes in DB: %v", err)
	}

	return
}

func (fsh *StorageHandler) DownloadFile(ctx context.Context, r *http.Request) (
	resp interface{}, err error) {

	if r.Method == "GET" {
		return nil, common.NewError("download_file",
			"invalid method used (GET), use POST instead")
	}

	var (
		allocationTx = ctx.Value(constants.ALLOCATION_CONTEXT_KEY).(string)
		clientID     = ctx.Value(constants.CLIENT_CONTEXT_KEY).(string)

		allocationObj *allocation.Allocation
	)

	if len(clientID) == 0 {
		return nil, common.NewError("download_file", "invalid client")
	}

	// runtime type check
	_ = ctx.Value(constants.CLIENT_KEY_CONTEXT_KEY).(string)

	// verify or update allocation
	allocationObj, err = fsh.verifyAllocation(ctx, allocationTx, false)
	if err != nil {
		return nil, common.NewErrorf("download_file",
			"invalid allocation id passed: %v", err)
	}

	var allocationID = allocationObj.ID

	if err = r.ParseMultipartForm(FORM_FILE_PARSE_MAX_MEMORY); nil != err {
		Logger.Info("download_file - request_parse_error", zap.Error(err))
		return nil, common.NewErrorf("download_file",
			"request_parse_error: %v", err)
	}

	var (
		pathHash = r.FormValue("path_hash")
		path     = r.FormValue("path")
	)

	if len(pathHash) == 0 {
		if len(path) == 0 {
			return nil, common.NewError("download_file", "invalid path")
		}
		pathHash = reference.GetReferenceLookup(allocationID, path)
	}

	var blockNumStr = r.FormValue("block_num")
	if len(blockNumStr) == 0 {
		return nil, common.NewError("download_file", "no block number")
	}

	var blockNum int64
	blockNum, err = strconv.ParseInt(blockNumStr, 10, 64)
	if err != nil || blockNum < 0 {
		return nil, common.NewError("download_file", "invalid block number")
	}

	var numBlocksStr = r.FormValue("num_blocks")
	if len(numBlocksStr) == 0 {
		numBlocksStr = "1"
	}

	var numBlocks int64
	numBlocks, err = strconv.ParseInt(numBlocksStr, 10, 64)
	if err != nil || numBlocks < 0 {
		return nil, common.NewError("download_file",
			"invalid number of blocks")
	}

	var (
		readMarkerString = r.FormValue("read_marker")
		readMarker       = &readmarker.ReadMarker{}
	)
	err = json.Unmarshal([]byte(readMarkerString), &readMarker)
	if err != nil {
		return nil, common.NewErrorf("download_file", "invalid parameters, "+
			"error parsing the readmarker for download: %v", err)
	}

	var rmObj = &readmarker.ReadMarkerEntity{}
	rmObj.LatestRM = readMarker

	if err = rmObj.VerifyMarker(ctx, allocationObj); err != nil {
		return nil, common.NewErrorf("download_file", "invalid read marker, "+
			"failed to verify the read marker: %v", err)
	}

	var fileref *reference.Ref
	fileref, err = reference.GetReferenceFromLookupHash(ctx, allocationID,
		pathHash)
	if err != nil {
		return nil, common.NewErrorf("download_file",
			"invalid file path: %v", err)
	}

	if fileref.Type != reference.FILE {
		return nil, common.NewErrorf("download_file",
			"path is not a file: %v", err)
	}

	var (
		authTokenString       = r.FormValue("auth_token")
		clientIDForReadRedeem = readMarker.ClientID
	)

	if (allocationObj.OwnerID != clientID &&
		allocationObj.PayerID != clientID) || len(authTokenString) > 0 {

		var authTicketVerified bool
		authTicketVerified, err = fsh.verifyAuthTicket(ctx, r, allocationObj,
			fileref, clientID)
		if err != nil {
			return nil, common.NewErrorf("download_file",
				"verifying auth ticket: %v", err)
		}

		if !authTicketVerified {
			return nil, common.NewErrorf("download_file",
				"could not verify the auth ticket")
		}

		var authToken = &readmarker.AuthTicket{}
		err = json.Unmarshal([]byte(authTokenString), &authToken)
		if err != nil {
			return nil, common.NewErrorf("download_file",
				"error parsing the auth ticket for download: %v", err)
		}

		clientIDForReadRedeem = authToken.OwnerID
	}

	var (
		rme           *readmarker.ReadMarkerEntity
		latestRM      *readmarker.ReadMarker
		pendNumBlocks int64
	)
	rme, err = readmarker.GetLatestReadMarkerEntity(ctx, clientID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, common.NewErrorf("download_file",
			"couldn't get read marker from DB: %v", err)
	}

	if rme != nil {
		latestRM = rme.LatestRM
		if pendNumBlocks, err = rme.PendNumBlocks(); err != nil {
			return nil, common.NewErrorf("download_file",
				"couldn't get number of blocks pending redeeming: %v", err)
		}
	}

	if latestRM != nil &&
		latestRM.ReadCounter+(numBlocks) != readMarker.ReadCounter {

		var response = &DownloadResponse{
			Success:      false,
			LatestRM:     latestRM,
			Path:         fileref.Path,
			AllocationID: fileref.AllocationID,
		}
		return response, nil
	}

	// check out read pool tokens if read_price > 0
	err = readPreRedeem(ctx, allocationObj, numBlocks, pendNumBlocks,
		clientIDForReadRedeem)
	if err != nil {
		return nil, common.NewErrorf("download_file",
			"pre-redeeming read marker: %v", err)
	}

	// reading allowed

	var (
		downloadMode = r.FormValue("content")
		respData     []byte
	)
	if len(downloadMode) > 0 && downloadMode == DOWNLOAD_CONTENT_THUMB {
		var fileData = &filestore.FileInputData{}
		fileData.Name = fileref.Name
		fileData.Path = fileref.Path
		fileData.Hash = fileref.ThumbnailHash
		fileData.OnCloud = fileref.OnCloud
		respData, err = filestore.GetFileStore().GetFileBlock(allocationID,
			fileData, blockNum, numBlocks)
		if err != nil {
			return nil, common.NewErrorf("download_file",
				"couldn't get thumbnail block: %v", err)
		}
	} else {
		var fileData = &filestore.FileInputData{}
		fileData.Name = fileref.Name
		fileData.Path = fileref.Path
		fileData.Hash = fileref.ContentHash
		fileData.OnCloud = fileref.OnCloud
		respData, err = filestore.GetFileStore().GetFileBlock(allocationID,
			fileData, blockNum, numBlocks)
		if err != nil {
			return nil, common.NewErrorf("download_file",
				"couldn't get file block: %v", err)
		}
	}

	err = readmarker.SaveLatestReadMarker(ctx, readMarker, latestRM == nil)
	if err != nil {
		return nil, common.NewErrorf("download_file",
			"couldn't save latest read marker: %v", err)
	}

	var response = &DownloadResponse{}
	response.Success = true
	response.LatestRM = readMarker
	response.Data = respData
	response.Path = fileref.Path
	response.AllocationID = fileref.AllocationID

	stats.FileBlockDownloaded(ctx, fileref.ID)
	return respData, nil
}

func (fsh *StorageHandler) CommitWrite(ctx context.Context, r *http.Request) (*CommitResult, error) {

	if r.Method == "GET" {
		return nil, common.NewError("invalid_method", "Invalid method used for the upload URL. Use POST instead")
	}
	allocationTx := ctx.Value(constants.ALLOCATION_CONTEXT_KEY).(string)
	clientID := ctx.Value(constants.CLIENT_CONTEXT_KEY).(string)
	clientKey := ctx.Value(constants.CLIENT_KEY_CONTEXT_KEY).(string)
	clientKeyBytes, _ := hex.DecodeString(clientKey)

	allocationObj, err := fsh.verifyAllocation(ctx, allocationTx, false)
	if err != nil {
		return nil, common.NewError("invalid_parameters", "Invalid allocation id passed."+err.Error())
	}

	allocationID := allocationObj.ID

	if len(clientID) == 0 || allocationObj.OwnerID != clientID || len(clientKey) == 0 || encryption.Hash(clientKeyBytes) != clientID {
		return nil, common.NewError("invalid_operation", "Operation needs to be performed by the owner of the allocation")
	}

	if err = r.ParseMultipartForm(FORM_FILE_PARSE_MAX_MEMORY); nil != err {
		Logger.Info("Error Parsing the request", zap.Any("error", err))
		return nil, common.NewError("request_parse_error", err.Error())
	}

	connectionID := r.FormValue("connection_id")
	if len(connectionID) == 0 {
		return nil, common.NewError("invalid_parameters", "Invalid connection id passed")
	}

	mutex := lock.GetMutex(allocationObj.TableName(), allocationID)
	mutex.Lock()
	defer mutex.Unlock()

	connectionObj, err := allocation.GetAllocationChanges(ctx, connectionID, allocationID, clientID)
	if err != nil {
		return nil, common.NewError("invalid_parameters", "Invalid connection id. Connection id was not found. "+err.Error())
	}
	if len(connectionObj.Changes) == 0 {
		return nil, common.NewError("invalid_parameters", "Invalid connection id. Connection does not have any changes.")
	}

	if allocationObj.BlobberSizeUsed+connectionObj.Size > allocationObj.BlobberSize {
		return nil, common.NewError("max_allocation_size", "Max size reached for the allocation with this blobber")
	}

	writeMarkerString := r.FormValue("write_marker")
	writeMarker := writemarker.WriteMarker{}
	err = json.Unmarshal([]byte(writeMarkerString), &writeMarker)
	if err != nil {
		return nil, common.NewError("invalid_parameters", "Invalid parameters. Error parsing the writemarker for commit."+err.Error())
	}

	var result CommitResult
	var latestWM *writemarker.WriteMarkerEntity
	if len(allocationObj.AllocationRoot) == 0 {
		latestWM = nil
	} else {
		latestWM, err = writemarker.GetWriteMarkerEntity(ctx, allocationObj.AllocationRoot)
		if err != nil {
			return nil, common.NewError("latest_write_marker_read_error", "Error reading the latest write marker for allocation."+err.Error())
		}
	}

	writemarkerObj := &writemarker.WriteMarkerEntity{}
	writemarkerObj.WM = writeMarker

	err = writemarkerObj.VerifyMarker(ctx, allocationObj, connectionObj)
	if err != nil {
		result.AllocationRoot = allocationObj.AllocationRoot
		result.ErrorMessage = "Verification of write marker failed. " + err.Error()
		result.Success = false
		if latestWM != nil {
			result.WriteMarker = &latestWM.WM
		}
		return &result, common.NewError("write_marker_verification_failed", result.ErrorMessage)
	}

	if err = writePreRedeem(ctx, allocationObj, &writeMarker); err != nil {
		return nil, err
	}

	err = connectionObj.ApplyChanges(ctx, writeMarker.AllocationRoot)
	if err != nil {
		return nil, err
	}
	rootRef, err := reference.GetReference(ctx, allocationID, "/")
	if err != nil {
		return nil, err
	}
	allocationRoot := encryption.Hash(rootRef.Hash + ":" + strconv.FormatInt(int64(writeMarker.Timestamp), 10))

	if allocationRoot != writeMarker.AllocationRoot {
		result.AllocationRoot = allocationObj.AllocationRoot
		if latestWM != nil {
			result.WriteMarker = &latestWM.WM
		}
		result.Success = false
		result.ErrorMessage = "Allocation root in the write marker does not match the calculated allocation root. Expected hash: " + allocationRoot
		return &result, common.NewError("allocation_root_mismatch", result.ErrorMessage)
	}
	writemarkerObj.ConnectionID = connectionObj.ConnectionID
	writemarkerObj.ClientPublicKey = clientKey
	err = writemarkerObj.Save(ctx)
	if err != nil {
		return nil, common.NewError("write_marker_error", "Error persisting the write marker")
	}

	db := datastore.GetStore().GetTransaction(ctx)
	allocationUpdates := make(map[string]interface{})
	allocationUpdates["blobber_size_used"] = gorm.Expr("blobber_size_used + ?", connectionObj.Size)
	allocationUpdates["used_size"] = gorm.Expr("used_size + ?", connectionObj.Size)
	allocationUpdates["allocation_root"] = allocationRoot
	allocationUpdates["is_redeem_required"] = true

	err = db.Model(allocationObj).Updates(allocationUpdates).Error
	if err != nil {
		return nil, common.NewError("allocation_write_error", "Error persisting the allocation object")
	}
	err = connectionObj.CommitToFileStore(ctx)
	if err != nil {
		return nil, common.NewError("file_store_error", "Error committing to file store. "+err.Error())
	}

	result.Changes = connectionObj.Changes

	connectionObj.DeleteChanges(ctx)

	db.Model(connectionObj).Updates(allocation.AllocationChangeCollector{Status: allocation.CommittedConnection})

	result.AllocationRoot = allocationObj.AllocationRoot
	result.WriteMarker = &writeMarker
	result.Success = true
	result.ErrorMessage = ""

	return &result, nil
}

func (fsh *StorageHandler) RenameObject(ctx context.Context, r *http.Request) (interface{}, error) {

	if r.Method == "GET" {
		return nil, common.NewError("invalid_method", "Invalid method used. Use POST instead")
	}
	allocationTx := ctx.Value(constants.ALLOCATION_CONTEXT_KEY).(string)
	allocationObj, err := fsh.verifyAllocation(ctx, allocationTx, false)
	clientID := ctx.Value(constants.CLIENT_CONTEXT_KEY).(string)
	_ = ctx.Value(constants.CLIENT_KEY_CONTEXT_KEY).(string)
	if err != nil {
		return nil, common.NewError("invalid_parameters", "Invalid allocation id passed."+err.Error())
	}

	allocationID := allocationObj.ID

	if len(clientID) == 0 {
		return nil, common.NewError("invalid_operation", "Invalid client")
	}

	new_name := r.FormValue("new_name")
	if len(new_name) == 0 {
		return nil, common.NewError("invalid_parameters", "Invalid name")
	}

	path_hash := r.FormValue("path_hash")
	path := r.FormValue("path")
	if len(path_hash) == 0 {
		if len(path) == 0 {
			return nil, common.NewError("invalid_parameters", "Invalid path")
		}
		path_hash = reference.GetReferenceLookup(allocationID, path)
	}
	if len(clientID) == 0 || allocationObj.OwnerID != clientID {
		return nil, common.NewError("invalid_operation", "Operation needs to be performed by the owner of the allocation")
	}

	connectionID := r.FormValue("connection_id")
	if len(connectionID) == 0 {
		return nil, common.NewError("invalid_parameters", "Invalid connection id passed")
	}

	connectionObj, err := allocation.GetAllocationChanges(ctx, connectionID, allocationID, clientID)
	if err != nil {
		return nil, common.NewError("meta_error", "Error reading metadata for connection")
	}

	mutex := lock.GetMutex(connectionObj.TableName(), connectionID)
	mutex.Lock()
	defer mutex.Unlock()

	objectRef, err := reference.GetReferenceFromLookupHash(ctx, allocationID, path_hash)

	if err != nil {
		return nil, common.NewError("invalid_parameters", "Invalid file path. "+err.Error())
	}

	allocationChange := &allocation.AllocationChange{}
	allocationChange.ConnectionID = connectionObj.ConnectionID
	allocationChange.Size = 0
	allocationChange.Operation = allocation.RENAME_OPERATION
	dfc := &allocation.RenameFileChange{ConnectionID: connectionObj.ConnectionID,
		AllocationID: connectionObj.AllocationID, Path: objectRef.Path}
	dfc.NewName = new_name
	connectionObj.Size += allocationChange.Size
	connectionObj.AddChange(allocationChange, dfc)

	err = connectionObj.Save(ctx)
	if err != nil {
		Logger.Error("Error in writing the connection meta data", zap.Error(err))
		return nil, common.NewError("connection_write_error", "Error writing the connection meta data")
	}

	result := &UploadResult{}
	result.Filename = new_name
	result.Hash = objectRef.Hash
	result.MerkleRoot = objectRef.MerkleRoot
	result.Size = objectRef.Size

	return result, nil
}

func (fsh *StorageHandler) CopyObject(ctx context.Context, r *http.Request) (interface{}, error) {
	if r.Method == "GET" {
		return nil, common.NewError("invalid_method", "Invalid method used. Use POST instead")
	}
	allocationTx := ctx.Value(constants.ALLOCATION_CONTEXT_KEY).(string)
	allocationObj, err := fsh.verifyAllocation(ctx, allocationTx, false)
	clientID := ctx.Value(constants.CLIENT_CONTEXT_KEY).(string)
	_ = ctx.Value(constants.CLIENT_KEY_CONTEXT_KEY).(string)
	if err != nil {
		return nil, common.NewError("invalid_parameters", "Invalid allocation id passed."+err.Error())
	}

	allocationID := allocationObj.ID

	if len(clientID) == 0 {
		return nil, common.NewError("invalid_operation", "Invalid client")
	}

	destPath := r.FormValue("dest")
	if len(destPath) == 0 {
		return nil, common.NewError("invalid_parameters", "Invalid destination for operation")
	}

	path_hash := r.FormValue("path_hash")
	path := r.FormValue("path")
	if len(path_hash) == 0 {
		if len(path) == 0 {
			return nil, common.NewError("invalid_parameters", "Invalid path")
		}
		path_hash = reference.GetReferenceLookup(allocationID, path)
	}
	if len(clientID) == 0 || allocationObj.OwnerID != clientID {
		return nil, common.NewError("invalid_operation", "Operation needs to be performed by the owner of the allocation")
	}

	connectionID := r.FormValue("connection_id")
	if len(connectionID) == 0 {
		return nil, common.NewError("invalid_parameters", "Invalid connection id passed")
	}

	connectionObj, err := allocation.GetAllocationChanges(ctx, connectionID, allocationID, clientID)
	if err != nil {
		return nil, common.NewError("meta_error", "Error reading metadata for connection")
	}

	mutex := lock.GetMutex(connectionObj.TableName(), connectionID)
	mutex.Lock()
	defer mutex.Unlock()

	objectRef, err := reference.GetReferenceFromLookupHash(ctx, allocationID, path_hash)

	if err != nil {
		return nil, common.NewError("invalid_parameters", "Invalid file path. "+err.Error())
	}
	newPath := filepath.Join(destPath, objectRef.Name)
	destRef, _ := reference.GetReference(ctx, allocationID, newPath)
	if destRef != nil {
		return nil, common.NewError("invalid_parameters", "Invalid destination path. Object Already exists.")
	}

	destRef, err = reference.GetReference(ctx, allocationID, destPath)
	if err != nil || destRef.Type != reference.DIRECTORY {
		return nil, common.NewError("invalid_parameters", "Invalid destination path. Should be a valid directory.")
	}

	allocationChange := &allocation.AllocationChange{}
	allocationChange.ConnectionID = connectionObj.ConnectionID
	allocationChange.Size = objectRef.Size
	allocationChange.Operation = allocation.COPY_OPERATION
	dfc := &allocation.CopyFileChange{ConnectionID: connectionObj.ConnectionID,
		AllocationID: connectionObj.AllocationID, DestPath: destPath}
	dfc.SrcPath = objectRef.Path
	connectionObj.Size += allocationChange.Size
	connectionObj.AddChange(allocationChange, dfc)

	err = connectionObj.Save(ctx)
	if err != nil {
		Logger.Error("Error in writing the connection meta data", zap.Error(err))
		return nil, common.NewError("connection_write_error", "Error writing the connection meta data")
	}

	result := &UploadResult{}
	result.Filename = objectRef.Name
	result.Hash = objectRef.Hash
	result.MerkleRoot = objectRef.MerkleRoot
	result.Size = objectRef.Size

	return result, nil
}

func (fsh *StorageHandler) DeleteFile(ctx context.Context, r *http.Request, connectionObj *allocation.AllocationChangeCollector) (*UploadResult, error) {
	path := r.FormValue("path")
	if len(path) == 0 {
		return nil, common.NewError("invalid_parameters", "Invalid path")
	}

	fileRef, _ := reference.GetReference(ctx, connectionObj.AllocationID, path)
	_ = ctx.Value(constants.CLIENT_KEY_CONTEXT_KEY).(string)
	if fileRef != nil {
		deleteSize := fileRef.Size

		allocationChange := &allocation.AllocationChange{}
		allocationChange.ConnectionID = connectionObj.ConnectionID
		allocationChange.Size = 0 - deleteSize
		allocationChange.Operation = allocation.DELETE_OPERATION
		dfc := &allocation.DeleteFileChange{ConnectionID: connectionObj.ConnectionID,
			AllocationID: connectionObj.AllocationID, Name: fileRef.Name,
			Hash: fileRef.Hash, Path: fileRef.Path, Size: deleteSize}

		connectionObj.Size += allocationChange.Size
		connectionObj.AddChange(allocationChange, dfc)

		result := &UploadResult{}
		result.Filename = fileRef.Name
		result.Hash = fileRef.Hash
		result.MerkleRoot = fileRef.MerkleRoot
		result.Size = fileRef.Size

		return result, nil
	}

	return nil, common.NewError("invalid_file", "File does not exist at path")
}

//WriteFile stores the file into the blobber files system from the HTTP request
func (fsh *StorageHandler) WriteFile(ctx context.Context, r *http.Request) (*UploadResult, error) {

	if r.Method == "GET" {
		return nil, common.NewError("invalid_method", "Invalid method used for the upload URL. Use multi-part form POST / PUT / DELETE instead")
	}

	allocationTx := ctx.Value(constants.ALLOCATION_CONTEXT_KEY).(string)
	clientID := ctx.Value(constants.CLIENT_CONTEXT_KEY).(string)

	allocationObj, err := fsh.verifyAllocation(ctx, allocationTx, false)
	if err != nil {
		return nil, common.NewError("invalid_parameters", "Invalid allocation id passed."+err.Error())
	}

	allocationID := allocationObj.ID

	if len(clientID) == 0 || (allocationObj.OwnerID != clientID && allocationObj.PayerID != clientID) {
		return nil, common.NewError("invalid_operation", "Operation needs to be performed by the owner or the payer of the allocation")
	}

	if err = r.ParseMultipartForm(FORM_FILE_PARSE_MAX_MEMORY); nil != err {
		Logger.Info("Error Parsing the request", zap.Any("error", err))
		return nil, common.NewError("request_parse_error", err.Error())
	}

	connectionID := r.FormValue("connection_id")
	if len(connectionID) == 0 {
		return nil, common.NewError("invalid_parameters", "Invalid connection id passed")
	}

	connectionObj, err := allocation.GetAllocationChanges(ctx, connectionID, allocationID, allocationObj.OwnerID)
	if err != nil {
		return nil, common.NewError("meta_error", "Error reading metadata for connection")
	}

	mutex := lock.GetMutex(connectionObj.TableName(), connectionID)
	mutex.Lock()
	defer mutex.Unlock()

	result := &UploadResult{}
	mode := allocation.INSERT_OPERATION
	if r.Method == "PUT" {
		mode = allocation.UPDATE_OPERATION
	} else if r.Method == "DELETE" {
		mode = allocation.DELETE_OPERATION
	}
	if mode == allocation.DELETE_OPERATION {
		result, err = fsh.DeleteFile(ctx, r, connectionObj)
		if err != nil {
			return nil, err
		}
	} else if mode == allocation.INSERT_OPERATION || mode == allocation.UPDATE_OPERATION {
		var formData allocation.UpdateFileChange
		formField := "uploadMeta"
		if mode == allocation.UPDATE_OPERATION {
			formField = "updateMeta"
		}
		uploadMetaString := r.FormValue(formField)
		err = json.Unmarshal([]byte(uploadMetaString), &formData)
		if err != nil {
			return nil, common.NewError("invalid_parameters",
				"Invalid parameters. Error parsing the meta data for upload."+err.Error())
		}
		exisitingFileRef := fsh.checkIfFileAlreadyExists(ctx, allocationID, formData.Path)
		existingFileRefSize := int64(0)
		exisitingFileOnCloud := false
		if mode == allocation.INSERT_OPERATION && exisitingFileRef != nil {
			return nil, common.NewError("duplicate_file", "File at path already exists")
		} else if mode == allocation.UPDATE_OPERATION && exisitingFileRef == nil {
			return nil, common.NewError("invalid_file_update", "File at path does not exist for update")
		}

		if exisitingFileRef != nil {
			existingFileRefSize = exisitingFileRef.Size
			exisitingFileOnCloud = exisitingFileRef.OnCloud
		}

		origfile, _, err := r.FormFile("uploadFile")
		if err != nil {
			return nil, common.NewError("invalid_parameters", "Error Reading multi parts for file."+err.Error())
		}
		defer origfile.Close()

		thumbfile, thumbHeader, _ := r.FormFile("uploadThumbnailFile")
		thumbnailPresent := false
		if thumbHeader != nil {
			thumbnailPresent = true
			defer thumbfile.Close()
		}

		fileInputData := &filestore.FileInputData{Name: formData.Filename, Path: formData.Path, OnCloud: exisitingFileOnCloud}
		fileOutputData, err := filestore.GetFileStore().WriteFile(allocationID, fileInputData, origfile, connectionObj.ConnectionID)
		if err != nil {
			return nil, common.NewError("upload_error", "Failed to upload the file. "+err.Error())
		}

		result.Filename = formData.Filename
		result.Hash = fileOutputData.ContentHash
		result.MerkleRoot = fileOutputData.MerkleRoot
		result.Size = fileOutputData.Size

		if len(formData.Hash) > 0 && formData.Hash != fileOutputData.ContentHash {
			return nil, common.NewError("content_hash_mismatch", "Content hash provided in the meta data does not match the file content")
		}
		if len(formData.MerkleRoot) > 0 && formData.MerkleRoot != fileOutputData.MerkleRoot {
			return nil, common.NewError("content_merkle_root_mismatch", "Merkle root provided in the meta data does not match the file content")
		}

		formData.Hash = fileOutputData.ContentHash
		formData.MerkleRoot = fileOutputData.MerkleRoot
		formData.AllocationID = allocationID
		formData.Size = fileOutputData.Size

		allocationSize := fileOutputData.Size
		if thumbnailPresent {
			thumbInputData := &filestore.FileInputData{Name: thumbHeader.Filename, Path: formData.Path}
			thumbOutputData, err := filestore.GetFileStore().WriteFile(allocationID, thumbInputData, thumbfile, connectionObj.ConnectionID)
			if err != nil {
				return nil, common.NewError("upload_error", "Failed to upload the thumbnail. "+err.Error())
			}
			if len(formData.ThumbnailHash) > 0 && formData.ThumbnailHash != thumbOutputData.ContentHash {
				return nil, common.NewError("content_hash_mismatch", "Content hash provided in the meta data does not match the thumbnail content")
			}
			formData.ThumbnailHash = thumbOutputData.ContentHash
			formData.ThumbnailSize = thumbOutputData.Size
			formData.ThumbnailFilename = thumbInputData.Name
		}

		if allocationObj.BlobberSizeUsed+(allocationSize-existingFileRefSize) > allocationObj.BlobberSize {
			return nil, common.NewError("max_allocation_size", "Max size reached for the allocation with this blobber")
		}

		allocationChange := &allocation.AllocationChange{}
		allocationChange.ConnectionID = connectionObj.ConnectionID
		allocationChange.Size = allocationSize - existingFileRefSize
		allocationChange.Operation = mode

		connectionObj.Size += allocationChange.Size
		if mode == allocation.INSERT_OPERATION {
			connectionObj.AddChange(allocationChange, &formData.NewFileChange)
		} else if mode == allocation.UPDATE_OPERATION {
			connectionObj.AddChange(allocationChange, &formData)
		}
	}
	err = connectionObj.Save(ctx)
	if err != nil {
		Logger.Error("Error in writing the connection meta data", zap.Error(err))
		return nil, common.NewError("connection_write_error", "Error writing the connection meta data")
	}

	return result, nil
}
