package zcn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/minio/cli"
	"github.com/minio/madmin-go"
	minio "github.com/minio/minio/cmd"
)

const (
	rootPath       = "/"
	rootBucketName = "root"
)

var configDir string
var allocationID string
var nonce int64

var zFlags = []cli.Flag{
	cli.StringFlag{
		Name:        "configDir",
		Usage:       "Config directory containing config.yaml, wallet.json, allocation.txt, etc.",
		Destination: &configDir,
	},
	cli.StringFlag{
		Name:        "allocationId",
		Usage:       "Allocation id of an allocation",
		Destination: &allocationID,
	},
	cli.Int64Flag{
		Name:        "nonce",
		Usage:       "nonce to use in transaction",
		Destination: &nonce,
	},
}

func init() {
	const zcnGateWayTemplate = `NAME:
	{{.HelpName}} - {{.Usage}}

  USAGE:
	{{.HelpName}} {{if .VisibleFlags}}[FLAGS]{{end}} ZCN-NAMENODE [ZCN-NAMENODE...]
  {{if .VisibleFlags}}
  FLAGS:
	{{range .VisibleFlags}}{{.}}
	{{end}}{{end}}
  ZCN-NAMENODE:
	ZCN namenode URI

  EXAMPLES:
	1. Start minio gateway server for ZeroChain backend
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_USER{{.AssignmentOperator}}accesskey
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_PASSWORD{{.AssignmentOperator}}secretkey
	   {{.Prompt}} {{.HelpName}} zcn://namenode:8200

	2. Start minio gateway server for ZCN with edge caching enabled
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_USER{{.AssignmentOperator}}accesskey
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_PASSWORD{{.AssignmentOperator}}secretkey
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_DRIVES{{.AssignmentOperator}}"/mnt/drive1,/mnt/drive2,/mnt/drive3,/mnt/drive4"
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_EXCLUDE{{.AssignmentOperator}}"bucket1/*,*.png"
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_QUOTA{{.AssignmentOperator}}90
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_AFTER{{.AssignmentOperator}}3
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_WATERMARK_LOW{{.AssignmentOperator}}75
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_WATERMARK_HIGH{{.AssignmentOperator}}85
	   {{.Prompt}} {{.HelpName}} hdfs://namenode:8200
  `

	minio.RegisterGatewayCommand(cli.Command{
		Name:               minio.ZCNBAckendGateway,
		Usage:              "0chain dStorage",
		Action:             zcnGatewayMain,
		CustomHelpTemplate: zcnGateWayTemplate,
		Flags:              zFlags,
		HideHelpCommand:    true,
	})
}

func zcnGatewayMain(ctx *cli.Context) {
	if ctx.Args().First() == "help" {
		cli.ShowCommandHelpAndExit(ctx, minio.ZCNBAckendGateway, 1)
	}

	minio.StartGateway(ctx, &ZCN{args: ctx.Args()})
}

// ZCN implements gateway
type ZCN struct {
	args []string
}

// Name implements gateway interface
func (z *ZCN) Name() string {
	return minio.ZCNBAckendGateway
}

// NewGatewayLayer initializes 0chain gosdk and return zcnObjects
func (z *ZCN) NewGatewayLayer(creds madmin.Credentials) (minio.ObjectLayer, error) {
	err := initializeSDK(configDir, allocationID, nonce)
	if err != nil {
		return nil, err
	}

	allocation, err := sdk.GetAllocation(allocationID)
	if err != nil {
		return nil, err
	}

	zob := &zcnObjects{
		alloc:   allocation,
		metrics: minio.NewMetrics(),
	}

	return zob, nil
}

type zcnObjects struct {
	minio.GatewayUnsupported
	alloc   *sdk.Allocation
	metrics *minio.BackendMetrics
}

// Shutdown Remove temporary directory
func (zob *zcnObjects) Shutdown(ctx context.Context) error {
	os.RemoveAll(tempdir)
	return nil
}

func (zob *zcnObjects) Production() bool {
	return true
}

func (zob *zcnObjects) GetMetrics(ctx context.Context) (*minio.BackendMetrics, error) {
	return zob.metrics, nil
}

// DeleteBucket Delete only empty bucket unless forced
func (zob *zcnObjects) DeleteBucket(ctx context.Context, bucketName string, opts minio.DeleteBucketOptions) error {
	if bucketName == rootBucketName {
		return errors.New("cannot remove root path")
	}

	remotePath := filepath.Join(rootPath, bucketName)

	ref, err := getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		return err
	}

	if ref.Type != dirType {
		return fmt.Errorf("%v is object not bucket", bucketName)
	}

	if opts.Force {
		return zob.alloc.DeleteFile(remotePath)
	}

	if ref.Size > 0 {
		return fmt.Errorf("%v bucket is not empty", bucketName)
	}

	return zob.alloc.DeleteFile(remotePath)
}

func (zob *zcnObjects) DeleteObject(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (oInfo minio.ObjectInfo, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		return
	}

	err = zob.alloc.DeleteFile(remotePath)
	if err != nil {
		return
	}

	return minio.ObjectInfo{
		Bucket:  bucket,
		Name:    ref.Name,
		ModTime: time.Now(),
		Size:    ref.ActualFileSize,
		IsDir:   ref.Type == dirType,
	}, nil
}

func (zob *zcnObjects) DeleteObjects(ctx context.Context, bucket string, objects []minio.ObjectToDelete, opts minio.ObjectOptions) (delObs []minio.DeletedObject, errs []error) {
	var basePath string
	if bucket == rootBucketName {
		basePath = rootPath
	} else {
		basePath = filepath.Join(rootPath, bucket)
	}

	for _, object := range objects {
		remotePath := filepath.Join(basePath, object.ObjectName)
		err := zob.alloc.DeleteFile(remotePath)
		if err != nil {
			errs = append(errs, err)
		} else {
			delObs = append(delObs, minio.DeletedObject{
				ObjectName: object.ObjectName,
			})
		}
	}
	return
}

// GetBucketInfo Get directory's metadata and present it as minio.BucketInfo
func (zob *zcnObjects) GetBucketInfo(ctx context.Context, bucket string) (bi minio.BucketInfo, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = rootPath
	} else {
		remotePath = filepath.Join(rootPath, bucket)
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		if isPathNoExistError(err) {
			if remotePath == rootPath {
				return minio.BucketInfo{Name: rootBucketName}, nil
			}
			return bi, minio.BucketNotFound{Bucket: bucket}
		}
		return
	}

	return minio.BucketInfo{Name: ref.Name, Created: ref.CreatedAt.ToTime()}, nil
}

// GetObjectInfo Get file meta data and respond it as minio.ObjectInfo
func (zob *zcnObjects) GetObjectInfo(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		if isPathNoExistError(err) {
			return objInfo, minio.ObjectNotFound{Bucket: bucket, Object: object}
		}
		return
	}

	return minio.ObjectInfo{
		Bucket:      bucket,
		Name:        getCommonPrefix(remotePath),
		ModTime:     ref.UpdatedAt.ToTime(),
		Size:        ref.ActualFileSize,
		IsDir:       ref.Type == dirType,
		AccTime:     time.Now(),
		ContentType: ref.MimeType,
	}, nil
}

// GetObjectNInfo Provides reader with read cursor placed at offset upto some length
func (zob *zcnObjects) GetObjectNInfo(ctx context.Context, bucket, object string, rs *minio.HTTPRangeSpec, h http.Header, lockType minio.LockType, opts minio.ObjectOptions) (gr *minio.GetObjectReader, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	ref, err := getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		if isPathNoExistError(err) {
			return nil, minio.ObjectNotFound{Bucket: bucket, Object: object}
		}
		return nil, err
	}

	objectInfo := minio.ObjectInfo{
		Bucket:  bucket,
		Name:    ref.Name,
		ModTime: ref.UpdatedAt.ToTime(),
		Size:    ref.ActualFileSize,
		IsDir:   ref.Type == dirType,
	}

	f, localPath, err := getFileReader(ctx, zob.alloc, remotePath, uint64(ref.ActualFileSize))
	fCloser := func() {
		f.Close()
		os.Remove(localPath)
	}
	if err != nil {
		return nil, err
	}

	finfo, err := f.Stat()
	if err != nil {
		return nil, err
	}

	startOffset, length, err := rs.GetOffsetLength(finfo.Size())
	if err != nil {
		return nil, err
	}

	r := io.NewSectionReader(f, startOffset, length)
	gr, err = minio.NewGetObjectReaderFromReader(r, objectInfo, opts, fCloser)
	return
}

// ListBuckets Lists directories of root path(/) and root path itself as buckets.
func (zob *zcnObjects) ListBuckets(ctx context.Context) (buckets []minio.BucketInfo, err error) {
	rootRef, err := getSingleRegularRef(zob.alloc, rootPath)
	if err != nil {
		if isPathNoExistError(err) {
			buckets = append(buckets, minio.BucketInfo{
				Name:    rootBucketName,
				Created: time.Now().Add(-time.Hour * 30),
			})
			return buckets, nil
		}
		return nil, err
	}

	dirRefs, err := listRootDir(zob.alloc, "d")
	if err != nil {
		return nil, err
	}

	// Consider root path as bucket as well.
	buckets = append(buckets, minio.BucketInfo{
		Name:    rootBucketName,
		Created: rootRef.CreatedAt.ToTime(),
	})

	for _, dirRef := range dirRefs {
		buckets = append(buckets, minio.BucketInfo{
			Name:    dirRef.Name,
			Created: dirRef.CreatedAt.ToTime(),
		})
	}
	return
}

func (zob *zcnObjects) ListObjectsV2(ctx context.Context, bucket, prefix, continuationToken, delimiter string, maxKeys int, fetchOwner bool, startAfter string) (result minio.ListObjectsV2Info, err error) {
	marker := continuationToken
	if marker == "" {
		marker = startAfter
	}

	var resultV1 minio.ListObjectsInfo
	resultV1, err = zob.ListObjects(ctx, bucket, prefix, marker, delimiter, maxKeys)
	if err != nil {
		return
	}

	result.Objects = resultV1.Objects
	result.Prefixes = resultV1.Prefixes
	result.ContinuationToken = continuationToken
	result.NextContinuationToken = resultV1.NextMarker
	result.IsTruncated = resultV1.IsTruncated
	return
}

// ListObjects Lists files of directories as objects
func (zob *zcnObjects) ListObjects(ctx context.Context, bucket, prefix, marker, delimiter string, maxKeys int) (result minio.ListObjectsInfo, err error) {
	// objFileType For root path list objects should only provide file and not dirs.
	// Dirs under root path are presented as buckets as well
	var remotePath, objFileType string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, prefix)
		objFileType = fileType
	} else {
		remotePath = filepath.Join(rootPath, bucket, prefix)
	}

	var isSuffix bool
	if strings.HasSuffix(prefix, "/") {
		remotePath = filepath.Clean(remotePath) + "/"
		isSuffix = true
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		if isPathNoExistError(err) {
			return result, nil
		}
		return
	}

	if ref.Type == fileType {
		if isSuffix {
			return minio.ListObjectsInfo{
					IsTruncated: false,
					Objects:     []minio.ObjectInfo{},
					Prefixes:    []string{},
				},
				nil
		}
		parentPath, fileName := filepath.Split(ref.Path)
		commonPrefix := getCommonPrefix(parentPath)
		objName := filepath.Join(commonPrefix, fileName)
		return minio.ListObjectsInfo{
				IsTruncated: false,
				Objects: []minio.ObjectInfo{
					{
						Bucket:       bucket,
						Name:         objName,
						Size:         ref.ActualFileSize,
						IsDir:        false,
						ModTime:      ref.UpdatedAt.ToTime(),
						ETag:         ref.ActualFileHash,
						ContentType:  ref.MimeType,
						AccTime:      time.Now(),
						StorageClass: "STANDARD",
					},
				},
				Prefixes: []string{},
			},
			nil
	}

	var objects []minio.ObjectInfo
	var isDelimited bool
	if delimiter != "" {
		isDelimited = true
	}

	refs, isTruncated, nextMarker, prefixes, err := listRegularRefs(zob.alloc, remotePath, marker, objFileType, maxKeys, isDelimited)
	if err != nil {
		if remotePath == rootPath && isPathNoExistError(err) {
			return minio.ListObjectsInfo{}, nil
		}
		return minio.ListObjectsInfo{}, err
	}

	for _, ref := range refs {
		if ref.Type == dirType {
			continue
		}

		objects = append(objects, minio.ObjectInfo{
			Bucket:       bucket,
			Name:         ref.Name,
			ModTime:      ref.UpdatedAt.ToTime(),
			Size:         ref.ActualFileSize,
			IsDir:        false,
			ContentType:  ref.MimeType,
			ETag:         ref.ActualFileHash,
			StorageClass: "STANDARD",
		})
	}

	result.IsTruncated = isTruncated
	result.NextMarker = nextMarker
	result.Objects = objects
	result.Prefixes = prefixes
	return
}

func (zob *zcnObjects) MakeBucketWithLocation(ctx context.Context, bucket string, opts minio.BucketOptions) error {
	// Create a directory; ignore opts
	remotePath := filepath.Join(rootPath, bucket)
	return zob.alloc.CreateDir(remotePath)
}

func (zob *zcnObjects) PutObject(ctx context.Context, bucket, object string, r *minio.PutObjReader, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	var isUpdate bool
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		if !isPathNoExistError(err) {
			return
		}
	}

	if ref != nil {
		isUpdate = true
	}

	contentType := opts.UserDefined["content-type"]
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	err = putFile(ctx, zob.alloc, remotePath, contentType, r, r.Size(), isUpdate, false)
	if err != nil {
		return
	}

	objInfo = minio.ObjectInfo{
		Bucket:  bucket,
		Name:    object,
		Size:    r.Size(),
		ModTime: time.Now(),
	}
	return
}

func (zob *zcnObjects) CopyObject(ctx context.Context, srcBucket, srcObject, destBucket, destObject string, srcInfo minio.ObjectInfo, srcOpts, dstOpts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	var srcRemotePath, dstRemotePath string
	if srcBucket == rootBucketName {
		srcRemotePath = filepath.Join(rootPath, srcObject)
	} else {
		srcRemotePath = filepath.Join(rootPath, srcBucket, srcObject)
	}

	if destBucket == rootBucketName {
		dstRemotePath = filepath.Join(rootPath, destObject)
	} else {
		dstRemotePath = filepath.Join(rootPath, destBucket, destObject)
	}

	err = zob.alloc.CopyObject(srcRemotePath, dstRemotePath)
	if err != nil {
		return
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, dstRemotePath)
	if err != nil {
		return
	}

	return minio.ObjectInfo{
		Bucket:  destBucket,
		Name:    destObject,
		ModTime: ref.UpdatedAt.ToTime(),
		Size:    ref.ActualFileSize,
	}, nil
}

func (zob *zcnObjects) StorageInfo(ctx context.Context) (si minio.StorageInfo, _ []error) {
	si.Backend.Type = madmin.Gateway
	si.Backend.GatewayOnline = true
	return
}

/*
//Unfortunately share file is done by minio client which does't need to communicate with server. It generates share url with access key id and
//secret key
func (zob *zcnObjects) ShareFile(ctx context.Context, bucket, object, clientID, pubEncryp string, expires, availableAfter time.Duration) (string, error) {
	var remotePath string
	if bucket == "" || (bucket == rootBucketName && object == "") {
		//share entire allocation i.e. rootpath
	} else if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	ref, err := getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		return "", err
	}

	_, fileName := filepath.Split(remotePath)

	authTicket, err := zob.alloc.GetAuthTicket(remotePath, fileName, ref.Type, clientID, pubEncryp, int64(expires.Seconds()), int64(availableAfter.Seconds()))
	if err != nil {
		return "", err
	}

	_ = authTicket
	//get public url from 0NFT
	return "", nil
}

func (zob *zcnObjects) RevokeShareCredential(ctx context.Context, bucket, object, clientID string) (err error) {
	var remotePath string
	if bucket == "" || (bucket == rootBucketName && object == "") {
		//share entire allocation i.e. rootpath
	} else if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	_, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		return
	}

	return zob.alloc.RevokeShare(remotePath, clientID)
}
*/
