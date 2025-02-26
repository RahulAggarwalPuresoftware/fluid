/*
Copyright 2022 The Fluid Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package jindofsx

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/fluid-cloudnative/fluid/pkg/utils/kubeclient"

	datav1alpha1 "github.com/fluid-cloudnative/fluid/api/v1alpha1"
	"github.com/fluid-cloudnative/fluid/pkg/common"
	"github.com/fluid-cloudnative/fluid/pkg/ddc/base/portallocator"
	"github.com/fluid-cloudnative/fluid/pkg/utils"
	"github.com/fluid-cloudnative/fluid/pkg/utils/docker"
	"github.com/fluid-cloudnative/fluid/pkg/utils/transfromer"
	corev1 "k8s.io/api/core/v1"
)

func (e *JindoFSxEngine) transform(runtime *datav1alpha1.JindoRuntime) (value *Jindo, err error) {
	if runtime == nil {
		err = fmt.Errorf("the jindoRuntime is null")
		return
	}

	dataset, err := utils.GetDataset(e.Client, e.name, e.namespace)
	if err != nil {
		return
	}

	var cachePaths []string // /mnt/disk1/bigboot or /mnt/disk1/bigboot,/mnt/disk2/bigboot
	var stroagePath = "/dev/shm/"
	if len(runtime.Spec.TieredStore.Levels) > 0 {
		stroagePath = runtime.Spec.TieredStore.Levels[0].Path
	}
	originPath := strings.Split(stroagePath, ",")
	for _, value := range originPath {
		cachePaths = append(cachePaths, strings.TrimRight(value, "/")+"/"+
			e.namespace+"/"+e.name+"/bigboot")
	}
	metaPath := cachePaths[0]
	dataPath := strings.Join(cachePaths, ",")

	var userSetQuota []string // 1Gi or 1Gi,2Gi,3Gi
	if len(runtime.Spec.TieredStore.Levels) == 0 {
		userSetQuota = append(userSetQuota, "1Gi")
	} else if runtime.Spec.TieredStore.Levels[0].Quota != nil {
		userSetQuota = append(userSetQuota, utils.TransformQuantityToJindoUnit(runtime.Spec.TieredStore.Levels[0].Quota))
	}

	if len(runtime.Spec.TieredStore.Levels) != 0 && runtime.Spec.TieredStore.Levels[0].QuotaList != "" {
		quotaList := runtime.Spec.TieredStore.Levels[0].QuotaList
		quotas := strings.Split(quotaList, ",")
		if len(quotas) != len(originPath) {
			err = fmt.Errorf("the num of cache path and quota must be equal")
			return
		}
		for _, value := range quotas {
			if strings.HasSuffix(value, "Gi") {
				value = strings.ReplaceAll(value, "Gi", "g")
			}
			userSetQuota = append(userSetQuota, value)
		}
	}
	userQuotas := strings.Join(userSetQuota, ",") // 1g or 1g,2g

	jindoSmartdataImage, smartdataTag, dnsServer := e.getSmartDataConfigs()
	jindoFuseImage, fuseTag := e.parseFuseImage()

	value = &Jindo{
		Image:           jindoSmartdataImage,
		ImageTag:        smartdataTag,
		ImagePullPolicy: "Always",
		FuseImage:       jindoFuseImage,
		FuseImageTag:    fuseTag,
		User:            0,
		Group:           0,
		FsGroup:         0,
		UseHostNetwork:  true,
		UseHostPID:      true,
		Properties:      e.transformPriority(metaPath),
		Master: Master{
			ReplicaCount: e.transformReplicasCount(runtime),
			NodeSelector: e.transformMasterSelector(runtime),
		},
		Worker: Worker{
			NodeSelector: e.transformNodeSelector(runtime),
		},
		Fuse: Fuse{
			Args:     e.transformFuseArg(runtime, dataset),
			HostPath: e.getHostMountPoint(),
		},
		Mounts: Mounts{
			Master:            e.transformMasterMountPath(metaPath),
			WorkersAndClients: e.transformWorkerMountPath(originPath),
		},
		Owner: transfromer.GenerateOwnerReferenceFromObject(runtime),
		RuntimeIdentity: RuntimeIdentity{
			Namespace: e.namespace,
			Name:      e.name,
		},
	}
	err = e.transformHadoopConfig(runtime, value)
	if err != nil {
		return
	}
	err = e.allocatePorts(value)
	if err != nil {
		return
	}
	e.transformNetworkMode(runtime, value)
	e.transformFuseNodeSelector(runtime, value)
	e.transformSecret(runtime, value)
	e.transformToken(runtime, value)
	err = e.transformMaster(runtime, metaPath, value, dataset)
	if err != nil {
		return
	}
	e.transformWorker(runtime, dataPath, userQuotas, value)
	e.transformFuse(runtime, value)
	e.transformInitPortCheck(value)
	e.transformLabels(runtime, value)
	e.transformPlacementMode(dataset, value)
	e.transformRunAsUser(runtime, value)
	e.transformTolerations(dataset, runtime, value)
	e.transformResources(runtime, value)
	e.transformLogConfig(runtime, value)
	e.transformDeployMode(runtime, value)
	value.Master.DnsServer = dnsServer
	value.Master.NameSpace = e.namespace
	value.Fuse.MountPath = JINDO_FUSE_MONNTPATH
	return value, err
}

func (e *JindoFSxEngine) transformMaster(runtime *datav1alpha1.JindoRuntime, metaPath string, value *Jindo, dataset *datav1alpha1.Dataset) (err error) {
	properties := map[string]string{
		"namespace.cluster.id":                      "local",
		"namespace.oss.copy.size":                   "1073741824",
		"namespace.filelet.threads":                 "10",
		"namespace.blocklet.threads":                "10",
		"namespace.long-running.threads":            "4",
		"namespace.filelet.cache.size":              "100000",
		"namespace.blocklet.cache.size":             "1000000",
		"namespace.filelet.atime.enable":            "false",
		"namespace.permission.root.inode.perm.bits": "511",
		"namespace.delete.scan.interval.second":     "20",
		"namespace.delete.scan.batch.size":          "5000",
		"namespace.backend.type":                    "rocksdb",
	}
	if value.Master.ReplicaCount == 3 {
		properties["namespace.backend.type"] = "raft"
	}
	properties["namespace.rpc.port"] = strconv.Itoa(value.Master.Port.Rpc)
	properties["namespace.meta-dir"] = metaPath + "/server"
	// combine properties together
	if len(runtime.Spec.Master.Properties) > 0 {
		for k, v := range runtime.Spec.Master.Properties {
			properties[k] = v
		}
	}
	value.Master.MasterProperties = properties
	// to set filestore properties with confvalue
	propertiesFileStore := map[string]string{}

	for _, mount := range dataset.Spec.Mounts {
		if !strings.HasSuffix(mount.MountPoint, "/") {
			mount.MountPoint = mount.MountPoint + "/"
		}
		// support nas storage
		if strings.HasPrefix(mount.MountPoint, "local:///") {
			value.Mounts.Master[mount.Name] = mount.MountPoint[8:]
			value.Mounts.WorkersAndClients[mount.Name] = mount.MountPoint[8:]
			continue
		}

		// TODO support cos storage
		if strings.HasPrefix(mount.MountPoint, "oss://") {
			var re = regexp.MustCompile(`(oss://(.*?))(/)`)
			rm := re.FindStringSubmatch(mount.MountPoint)
			if len(rm) < 3 {
				err = fmt.Errorf("incorrect oss mountPoint with %v, please check your path is dir or file ", mount.MountPoint)
				e.Log.Error(err, "mount.MountPoint", mount.MountPoint)
				return
			}
			bucketName := rm[2]
			if mount.Options["fs.oss.accessKeyId"] != "" {
				propertiesFileStore["jindofsx.oss.bucket."+bucketName+".accessKeyId"] = mount.Options["fs.oss.accessKeyId"]
			}
			if mount.Options["fs.oss.accessKeySecret"] != "" {
				propertiesFileStore["jindofsx.oss.bucket."+bucketName+".accessKeySecret"] = mount.Options["fs.oss.accessKeySecret"]
			}
			if mount.Options["fs.oss.endpoint"] == "" {
				err = fmt.Errorf("oss endpoint can not be null, please check <fs.oss.accessKeySecret> option")
				e.Log.Error(err, "oss endpoint can not be null")
				return
			}
			propertiesFileStore["jindofsx.oss.bucket."+bucketName+".endpoint"] = mount.Options["fs.oss.endpoint"]
			if strings.Contains(mount.Options["fs.oss.endpoint"], "dls") {
				propertiesFileStore["jindofsx.oss.bucket."+bucketName+".data.lake.storage.enable"] = "true"
			}
		}

		// support s3
		if strings.HasPrefix(mount.MountPoint, "s3://") {
			if mount.Options["fs.s3.accessKeyId"] != "" {
				propertiesFileStore["jindofsx.s3.accessKeyId"] = mount.Options["fs.s3.accessKeyId"]
			}
			if mount.Options["fs.s3.accessKeySecret"] != "" {
				propertiesFileStore["jindofsx.s3.accessKeySecret"] = mount.Options["fs.s3.accessKeySecret"]
			}
			if mount.Options["fs.s3.endpoint"] != "" {
				propertiesFileStore["jindofsx.s3.endpoint"] = mount.Options["fs.s3.endpoint"]
			}
			if mount.Options["fs.s3.region"] != "" {
				propertiesFileStore["jindofsx.s3.region"] = mount.Options["fs.s3.region"]
			}
		}

		// support cos
		if strings.HasPrefix(mount.MountPoint, "cos://") {
			if mount.Options["fs.cos.accessKeyId"] != "" {
				propertiesFileStore["jindofsx.cos.accessKeyId"] = mount.Options["fs.cos.accessKeyId"]
			}
			if mount.Options["fs.cos.accessKeySecret"] != "" {
				propertiesFileStore["jindofsx.cos.accessKeySecret"] = mount.Options["fs.cos.accessKeySecret"]
			}
			if mount.Options["fs.cos.endpoint"] != "" {
				propertiesFileStore["jindofsx.cos.endpoint"] = mount.Options["fs.cos.endpoint"]
			}
		}

		// support obs
		if strings.HasPrefix(mount.MountPoint, "obs://") {
			if mount.Options["fs.obs.accessKeyId"] != "" {
				propertiesFileStore["jindofsx.obs.accessKeyId"] = mount.Options["fs.obs.accessKeyId"]
			}
			if mount.Options["fs.obs.accessKeySecret"] != "" {
				propertiesFileStore["jindofsx.obs.accessKeySecret"] = mount.Options["fs.obs.accessKeySecret"]
			}
			if mount.Options["fs.obs.endpoint"] != "" {
				propertiesFileStore["jindofsx.obs.endpoint"] = mount.Options["fs.obs.endpoint"]
			}
		}

		// to check whether encryptOptions exist
		for _, encryptOption := range mount.EncryptOptions {
			key := encryptOption.Name
			secretKeyRef := encryptOption.ValueFrom.SecretKeyRef
			secret, err := kubeclient.GetSecret(e.Client, secretKeyRef.Name, e.namespace)
			if err != nil {
				e.Log.Info("can't get the secret")
				break
			}
			value := secret.Data[secretKeyRef.Key]
			if err != nil {
				e.Log.Info("decode value failed")
			}
			if key == "fs.oss.accessKeyId" {
				propertiesFileStore["jindofsx.oss.accessKeyId"] = string(value)
			}
			if key == "fs.oss.accessKeySecret" {
				propertiesFileStore["jindofsx.oss.accessKeySecret"] = string(value)
			}
			if key == "fs.s3.accessKeyId" {
				propertiesFileStore["jindofsx.s3.accessKeyId"] = string(value)
			}
			if key == "fs.s3.accessKeySecret" {
				propertiesFileStore["jindofsx.s3.accessKeySecret"] = string(value)
			}
			if key == "fs.cos.accessKeyId" {
				propertiesFileStore["jindofsx.cos.accessKeyId"] = string(value)
			}
			if key == "fs.cos.accessKeySecret" {
				propertiesFileStore["jindofsx.cos.accessKeySecret"] = string(value)
			}
			if key == "fs.obs.accessKeyId" {
				propertiesFileStore["jindofsx.obs.accessKeyId"] = string(value)
			}
			if key == "fs.obs.accessKeySecret" {
				propertiesFileStore["jindofsx.obs.accessKeySecret"] = string(value)
			}
			e.Log.Info("Get Credential From Secret Successfully")
		}
	}
	value.Master.FileStoreProperties = propertiesFileStore

	return nil
}

func (e *JindoFSxEngine) transformWorker(runtime *datav1alpha1.JindoRuntime, dataPath string, userQuotas string, value *Jindo) {

	properties := map[string]string{
		"storage.cluster.id":                   "local",
		"storage.compaction.enable":            "true",
		"storage.compaction.period.minute":     "2",
		"storage.maintainence.period.minute":   "2",
		"storage.compaction.threshold":         "16",
		"storage.cache.filelet.worker.threads": "200",
		"storage.address":                      "localhost",
	}

	if e.getTieredStoreType(runtime) == 0 {
		// MEM
		properties["storage.ram.cache.size"] = userQuotas
		//properties["storage.ram.cache.size"] = "90g"

		properties["storage.slicelet.buffer.size"] = userQuotas
		//properties["storage.slicelet.buffer.size"] = "90g"
	}

	properties["storage.rpc.port"] = strconv.Itoa(value.Worker.Port.Rpc)

	properties["storage.data-dirs"] = dataPath
	//properties["storage.data-dirs"] = "/mnt/disk1/bigboot, /mnt/disk2/bigboot, /mnt/disk3/bigboot"

	if len(runtime.Spec.TieredStore.Levels) == 0 {
		properties["storage.watermark.high.ratio"] = "0.8"
	} else {
		properties["storage.watermark.high.ratio"] = runtime.Spec.TieredStore.Levels[0].High
	}

	if len(runtime.Spec.TieredStore.Levels) == 0 {
		properties["storage.watermark.low.ratio"] = "0.6"
	} else {
		properties["storage.watermark.low.ratio"] = runtime.Spec.TieredStore.Levels[0].Low
	}

	properties["storage.data-dirs.capacities"] = userQuotas
	///properties["storage.data-dirs.capacities"] = "80g,80g,80g"

	if len(runtime.Spec.Worker.Properties) > 0 {
		for k, v := range runtime.Spec.Worker.Properties {
			properties[k] = v
		}
	}
	value.Worker.WorkerProperties = properties
}

func (e *JindoFSxEngine) transformResources(runtime *datav1alpha1.JindoRuntime, value *Jindo) {

	if runtime.Spec.Master.Resources.Limits != nil {
		e.Log.Info("setting Resources limit")
		if runtime.Spec.Master.Resources.Limits.Cpu() != nil {
			value.Master.Resources.Limits.CPU = runtime.Spec.Master.Resources.Limits.Cpu().String()
		}
		if runtime.Spec.Master.Resources.Limits.Memory() != nil {
			value.Master.Resources.Limits.Memory = runtime.Spec.Master.Resources.Limits.Memory().String()
		}
	}

	if runtime.Spec.Master.Resources.Requests != nil {
		e.Log.Info("setting Resources request")
		if runtime.Spec.Master.Resources.Requests.Cpu() != nil {
			value.Master.Resources.Requests.CPU = runtime.Spec.Master.Resources.Requests.Cpu().String()
		}
		if runtime.Spec.Master.Resources.Requests.Memory() != nil {
			value.Master.Resources.Requests.Memory = runtime.Spec.Master.Resources.Requests.Memory().String()
		}
	}

	if runtime.Spec.Fuse.Resources.Limits != nil {
		e.Log.Info("setting Resources limit")
		if runtime.Spec.Fuse.Resources.Limits.Cpu() != nil {
			value.Fuse.Resources.Limits.CPU = runtime.Spec.Fuse.Resources.Limits.Cpu().String()
		}
		if runtime.Spec.Fuse.Resources.Limits.Memory() != nil {
			value.Fuse.Resources.Limits.Memory = runtime.Spec.Fuse.Resources.Limits.Memory().String()
		}
	}

	if runtime.Spec.Fuse.Resources.Requests != nil {
		e.Log.Info("setting Resources request")
		if runtime.Spec.Fuse.Resources.Requests.Cpu() != nil {
			value.Fuse.Resources.Requests.CPU = runtime.Spec.Fuse.Resources.Requests.Cpu().String()
		}
		if runtime.Spec.Fuse.Resources.Requests.Memory() != nil {
			value.Fuse.Resources.Requests.Memory = runtime.Spec.Fuse.Resources.Requests.Memory().String()
		}
	}

	if runtime.Spec.Worker.Resources.Limits != nil {
		e.Log.Info("setting Resources limit")
		if runtime.Spec.Worker.Resources.Limits.Cpu() != nil {
			value.Worker.Resources.Limits.CPU = runtime.Spec.Worker.Resources.Limits.Cpu().String()
		}
		if runtime.Spec.Worker.Resources.Limits.Memory() != nil {
			value.Worker.Resources.Limits.Memory = runtime.Spec.Worker.Resources.Limits.Memory().String()
		}
	}

	if runtime.Spec.Worker.Resources.Requests != nil {
		e.Log.Info("setting Resources request")
		if runtime.Spec.Worker.Resources.Requests.Cpu() != nil {
			value.Worker.Resources.Requests.CPU = runtime.Spec.Worker.Resources.Requests.Cpu().String()
		}
		if runtime.Spec.Worker.Resources.Requests.Memory() != nil {
			value.Worker.Resources.Requests.Memory = runtime.Spec.Worker.Resources.Requests.Memory().String()
		}
	}
}

func (e *JindoFSxEngine) transformFuse(runtime *datav1alpha1.JindoRuntime, value *Jindo) {
	// default enable data-cache and disable meta-cache
	properties := map[string]string{
		"fs.jindofsx.request.user":           "root",
		"fs.jindofsx.data.cache.enable":      "true",
		"fs.jindofsx.meta.cache.enable":      "true",
		"fs.jindofsx.tmp.data.dir":           "/tmp",
		"fs.jindofsx.client.metrics.enable":  "true",
		"fs.oss.download.queue.size":         "16",
		"fs.oss.download.thread.concurrency": "32",
		"fs.s3.download.queue.size":          "16",
		"fs.s3.download.thread.concurrency":  "32",
	}

	for k, v := range value.Master.FileStoreProperties {
		// to transform jindofsx.oss.bucket to fs.jindofsx.oss.bucket
		properties[strings.Replace(k, "jindofsx", "fs", 1)] = v
	}

	// "client.storage.rpc.port": "6101",
	properties["fs.jindofsx.storage.rpc.port"] = strconv.Itoa(value.Worker.Port.Rpc)

	if e.getTieredStoreType(runtime) == 0 {
		// MEM
		properties["fs.jindofsx.ram.cache.enable"] = "true"
	} else if e.getTieredStoreType(runtime) == 1 || e.getTieredStoreType(runtime) == 2 {
		// HDD and SSD
		properties["fs.jindofsx.ram.cache.enable"] = "false"
	}
	// set secret
	if len(runtime.Spec.Secret) != 0 {
		properties["fs.oss.credentials.provider"] = "com.aliyun.jindodata.oss.auth.CustomCredentialsProvider"
		properties["aliyun.oss.provider.url"] = "secrets:///token/"
		properties["fs.oss.provider.endpoint"] = "secrets:///token/"
	}

	if len(runtime.Spec.Fuse.Properties) > 0 {
		for k, v := range runtime.Spec.Fuse.Properties {
			properties[k] = v
		}
	}
	value.Fuse.FuseProperties = properties

	// set critical fuse pod to avoid eviction
	value.Fuse.CriticalPod = common.CriticalFusePodEnabled()
}

func (e *JindoFSxEngine) transformLogConfig(runtime *datav1alpha1.JindoRuntime, value *Jindo) {
	if len(runtime.Spec.LogConfig) > 0 {
		value.LogConfig = runtime.Spec.LogConfig
	} else {
		properties := map[string]string{
			"logger.sync":    "false",
			"logger.verbose": "0",
		}
		value.LogConfig = properties
	}
}

func (e *JindoFSxEngine) transformFuseNodeSelector(runtime *datav1alpha1.JindoRuntime, value *Jindo) {
	if len(runtime.Spec.Fuse.NodeSelector) > 0 {
		value.Fuse.NodeSelector = runtime.Spec.Fuse.NodeSelector
	} else {
		value.Fuse.NodeSelector = map[string]string{}
	}

	// The label will be added by CSI Plugin when any workload pod is scheduled on the node.
	value.Fuse.NodeSelector[e.getFuseLabelname()] = "true"
}

func (e *JindoFSxEngine) transformNodeSelector(runtime *datav1alpha1.JindoRuntime) map[string]string {
	properties := map[string]string{}
	if runtime.Spec.Worker.NodeSelector != nil {
		properties = runtime.Spec.Worker.NodeSelector
	}
	return properties
}

func (e *JindoFSxEngine) transformReplicasCount(runtime *datav1alpha1.JindoRuntime) int {
	if runtime.Spec.Master.Replicas == JINDO_HA_MASTERNUM {
		return JINDO_HA_MASTERNUM
	}
	return JINDO_MASTERNUM_DEFAULT
}

func (e *JindoFSxEngine) transformMasterSelector(runtime *datav1alpha1.JindoRuntime) map[string]string {
	properties := map[string]string{}
	if runtime.Spec.Master.NodeSelector != nil {
		properties = runtime.Spec.Master.NodeSelector
	}
	return properties
}

func (e *JindoFSxEngine) transformPriority(metaPath string) map[string]string {
	properties := map[string]string{}
	properties["logDir"] = metaPath + "/log"
	return properties
}

func (e *JindoFSxEngine) transformMasterMountPath(metaPath string) map[string]string {
	properties := map[string]string{}
	properties["1"] = metaPath
	return properties
}

func (e *JindoFSxEngine) transformWorkerMountPath(originPath []string) map[string]string {
	properties := map[string]string{}
	for index, value := range originPath {
		properties[strconv.Itoa(index+1)] = strings.TrimRight(value, "/")
	}
	return properties
}

func (e *JindoFSxEngine) transformFuseArg(runtime *datav1alpha1.JindoRuntime, dataset *datav1alpha1.Dataset) []string {
	fuseArgs := []string{}
	readOnly := false
	runtimeInfo := e.runtimeInfo
	if runtimeInfo != nil {
		accessModes, err := utils.GetAccessModesOfDataset(e.Client, runtimeInfo.GetName(), runtimeInfo.GetNamespace())
		if err != nil {
			e.Log.Info("Error:", "err", err)
		}
		if len(accessModes) > 0 {
			for _, mode := range accessModes {
				if mode == corev1.ReadOnlyMany {
					readOnly = true
				}
			}
		}
	}
	if len(runtime.Spec.Fuse.Args) > 0 {
		fuseArgs = runtime.Spec.Fuse.Args
	} else {
		fuseArgs = append(fuseArgs, "-okernel_cache")
		if readOnly {
			fuseArgs = append(fuseArgs, "-oro")
			fuseArgs = append(fuseArgs, "-oattr_timeout=7200")
			fuseArgs = append(fuseArgs, "-oentry_timeout=7200")
		}
	}
	if runtime.Spec.Master.Disabled && runtime.Spec.Worker.Disabled {
		fuseArgs = append(fuseArgs, "-ouri="+dataset.Spec.Mounts[0].MountPoint)
	}
	return fuseArgs
}

func (e *JindoFSxEngine) getSmartDataConfigs() (image, tag, dnsServer string) {
	var (
		defaultImage     = "registry.cn-shanghai.aliyuncs.com/jindofs/smartdata"
		defaultTag       = "4.4.0"
		defaultDnsServer = "1.1.1.1"
	)

	image = docker.GetImageRepoFromEnv(common.JINDO_SMARTDATA_IMAGE_ENV)
	tag = docker.GetImageTagFromEnv(common.JINDO_SMARTDATA_IMAGE_ENV)
	dnsServer = os.Getenv(common.JINDO_DNS_SERVER)
	if len(image) == 0 {
		image = defaultImage
	}
	if len(tag) == 0 {
		tag = defaultTag
	}
	if len(dnsServer) == 0 {
		dnsServer = defaultDnsServer
	}
	e.Log.Info("Set image", "image", image, "tag", tag, "dnsServer", dnsServer)

	return
}

func (e *JindoFSxEngine) parseFuseImage() (image, tag string) {
	var (
		defaultImage = "registry.cn-shanghai.aliyuncs.com/jindofs/jindo-fuse"
		defaultTag   = "4.4.0"
	)

	image = docker.GetImageRepoFromEnv(common.JINDO_FUSE_IMAGE_ENV)
	tag = docker.GetImageTagFromEnv(common.JINDO_FUSE_IMAGE_ENV)
	if len(image) == 0 {
		image = defaultImage
	}
	if len(tag) == 0 {
		tag = defaultTag
	}
	e.Log.Info("Set image", "image", image, "tag", tag)

	return
}

func (e *JindoFSxEngine) transformSecret(runtime *datav1alpha1.JindoRuntime, value *Jindo) {
	if len(runtime.Spec.Secret) != 0 {
		value.Secret = runtime.Spec.Secret
	}
}

func (e *JindoFSxEngine) transformToken(runtime *datav1alpha1.JindoRuntime, value *Jindo) {
	properties := map[string]string{}
	if len(runtime.Spec.Secret) != 0 {
		properties["default.credential.provider"] = "secrets:///token/"
		properties["jindofsx.oss.provider.endpoint"] = "secrets:///token/"
	} else {
		properties["default.credential.provider"] = "none"
	}
	value.Master.TokenProperties = properties
}

func (e *JindoFSxEngine) allocatePorts(value *Jindo) error {

	// if not usehostnetwork then use default port
	// usehostnetwork to choose port from port allocator
	expectedPortNum := 2
	if !value.UseHostNetwork {
		value.Master.Port.Rpc = DEFAULT_MASTER_RPC_PORT
		value.Worker.Port.Rpc = DEFAULT_WORKER_RPC_PORT
		if value.Master.ReplicaCount == JINDO_HA_MASTERNUM {
			value.Master.Port.Raft = DEFAULT_RAFT_RPC_PORT
		}
		return nil
	}

	if value.Master.ReplicaCount == JINDO_HA_MASTERNUM {
		expectedPortNum = 3
	}

	allocator, err := portallocator.GetRuntimePortAllocator()
	if err != nil {
		e.Log.Error(err, "can't get runtime port allocator")
		return err
	}

	allocatedPorts, err := allocator.GetAvailablePorts(expectedPortNum)
	if err != nil {
		e.Log.Error(err, "can't get available ports", "expected port num", expectedPortNum)
		return err
	}

	index := 0
	value.Master.Port.Rpc = allocatedPorts[index]
	index++
	value.Worker.Port.Rpc = allocatedPorts[index]
	if value.Master.ReplicaCount == JINDO_HA_MASTERNUM {
		index++
		value.Master.Port.Raft = allocatedPorts[index]
	}
	return nil
}

func (e *JindoFSxEngine) transformInitPortCheck(value *Jindo) {
	// This function should be called after port allocation

	if !common.PortCheckEnabled() {
		return
	}

	e.Log.Info("Enabled port check")
	value.InitPortCheck.Enabled = true

	// Always use the default init image defined in env
	value.InitPortCheck.Image, value.InitPortCheck.ImageTag, value.InitPortCheck.ImagePullPolicy = docker.ParseInitImage("", "", "", common.DefaultInitImageEnv)

	// Inject ports to be checked to a init container which reports the usage status of the ports for easier debugging.
	// The jindo master container will always start even when some of the ports is in use.
	var ports []string

	ports = append(ports, strconv.Itoa(value.Master.Port.Rpc))
	if value.Master.ReplicaCount == JINDO_HA_MASTERNUM {
		ports = append(ports, strconv.Itoa(value.Master.Port.Raft))
	}

	// init container takes "PORT1:PORT2:PORT3..." as input
	value.InitPortCheck.PortsToCheck = strings.Join(ports, ":")
}

func (e *JindoFSxEngine) transformRunAsUser(runtime *datav1alpha1.JindoRuntime, value *Jindo) {
	if len(runtime.Spec.User) != 0 {
		value.Fuse.RunAs = runtime.Spec.User
	}
}

func (e *JindoFSxEngine) transformTolerations(dataset *datav1alpha1.Dataset, runtime *datav1alpha1.JindoRuntime, value *Jindo) {

	if len(dataset.Spec.Tolerations) > 0 {
		// value.Tolerations = dataset.Spec.Tolerations
		value.Tolerations = []corev1.Toleration{}
		for _, toleration := range dataset.Spec.Tolerations {
			toleration.TolerationSeconds = nil
			value.Tolerations = append(value.Tolerations, toleration)
		}
		value.Master.Tolerations = value.Tolerations
		value.Worker.Tolerations = value.Tolerations
		value.Fuse.Tolerations = value.Tolerations
	}

	if len(runtime.Spec.Master.Tolerations) > 0 {
		for _, toleration := range runtime.Spec.Master.Tolerations {
			toleration.TolerationSeconds = nil
			value.Master.Tolerations = append(value.Tolerations, toleration)
		}
	}

	if len(runtime.Spec.Worker.Tolerations) > 0 {
		for _, toleration := range runtime.Spec.Worker.Tolerations {
			toleration.TolerationSeconds = nil
			value.Worker.Tolerations = append(value.Tolerations, toleration)
		}
	}

	if len(runtime.Spec.Fuse.Tolerations) > 0 {
		for _, toleration := range runtime.Spec.Fuse.Tolerations {
			toleration.TolerationSeconds = nil
			value.Fuse.Tolerations = append(value.Tolerations, toleration)
		}
	}
}

func (e *JindoFSxEngine) transformLabels(runtime *datav1alpha1.JindoRuntime, value *Jindo) {
	// the labels will not be merged here because they will be sequentially added into yaml templates
	// If two labels share the same label key, the last one in yaml templates overrides the former ones
	// and takes effect.
	value.Labels = runtime.Spec.Labels
	value.Master.Labels = runtime.Spec.Master.Labels
	value.Worker.Labels = runtime.Spec.Worker.Labels
	value.Fuse.Labels = runtime.Spec.Fuse.Labels
}

func (e *JindoFSxEngine) transformNetworkMode(runtime *datav1alpha1.JindoRuntime, value *Jindo) {
	// to set hostnetwork
	switch runtime.Spec.NetworkMode {
	case datav1alpha1.HostNetworkMode:
		value.UseHostNetwork = true
	case datav1alpha1.ContainerNetworkMode:
		value.UseHostNetwork = false
	case datav1alpha1.DefaultNetworkMode:
		value.UseHostNetwork = true
	}
}

func (e *JindoFSxEngine) transformPlacementMode(dataset *datav1alpha1.Dataset, value *Jindo) {

	value.PlacementMode = string(dataset.Spec.PlacementMode)
	if len(value.PlacementMode) == 0 {
		value.PlacementMode = string(datav1alpha1.ExclusiveMode)
	}
}

func (e *JindoFSxEngine) transformDeployMode(runtime *datav1alpha1.JindoRuntime, value *Jindo) {
	// to set fuseOnly
	if runtime.Spec.Master.Disabled && runtime.Spec.Worker.Disabled {
		value.Master.ReplicaCount = 0
		value.Worker.ReplicaCount = 0
		value.Fuse.Mode = FuseOnly
	}
}
