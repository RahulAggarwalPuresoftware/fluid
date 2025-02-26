/*

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

package alluxio

import (
	"reflect"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	datav1alpha1 "github.com/fluid-cloudnative/fluid/api/v1alpha1"
	"github.com/fluid-cloudnative/fluid/pkg/utils/fake"
)

func TestTransformFuse(t *testing.T) {

	var x int64 = 1000
	ctrl.SetLogger(zap.New(func(o *zap.Options) {
		o.Development = true
	}))

	var tests = []struct {
		runtime *datav1alpha1.AlluxioRuntime
		dataset *datav1alpha1.Dataset
		value   *Alluxio
		expect  []string
	}{
		{&datav1alpha1.AlluxioRuntime{
			Spec: datav1alpha1.AlluxioRuntimeSpec{
				Fuse: datav1alpha1.AlluxioFuseSpec{},
			},
		}, &datav1alpha1.Dataset{
			Spec: datav1alpha1.DatasetSpec{
				Mounts: []datav1alpha1.Mount{{
					MountPoint: "local:///mnt/test",
					Name:       "test",
				}},
				Owner: &datav1alpha1.User{
					UID: &x,
					GID: &x,
				},
			},
		}, &Alluxio{}, []string{"fuse", "--fuse-opts=kernel_cache,rw,max_read=131072,uid=1000,gid=1000,allow_other"}},
	}
	for _, test := range tests {
		engine := &AlluxioEngine{}
		engine.Log = ctrl.Log
		err := engine.transformFuse(test.runtime, test.dataset, test.value)
		if err != nil {
			t.Errorf("error %v", err)
		}
		if test.value.Fuse.Args[1] != test.expect[1] {
			t.Errorf("expected %v, got %v", test.expect, test.value.Fuse.Args)
		}
	}
}

func TestTransformMaster(t *testing.T) {
	testCases := map[string]struct {
		runtime   *datav1alpha1.AlluxioRuntime
		wantValue *Alluxio
	}{
		"test network mode case 1": {
			runtime: &datav1alpha1.AlluxioRuntime{
				Spec: datav1alpha1.AlluxioRuntimeSpec{
					Master: datav1alpha1.AlluxioCompTemplateSpec{
						NetworkMode: datav1alpha1.ContainerNetworkMode,
					},
				},
			},
			wantValue: &Alluxio{
				Master: Master{
					HostNetwork: false,
				},
			},
		},
		"test network mode case 2": {
			runtime: &datav1alpha1.AlluxioRuntime{
				Spec: datav1alpha1.AlluxioRuntimeSpec{
					Master: datav1alpha1.AlluxioCompTemplateSpec{
						NetworkMode: datav1alpha1.HostNetworkMode,
					},
				},
			},
			wantValue: &Alluxio{
				Master: Master{
					HostNetwork: true,
				},
			},
		},
		"test network mode case 3": {
			runtime: &datav1alpha1.AlluxioRuntime{
				Spec: datav1alpha1.AlluxioRuntimeSpec{
					Master: datav1alpha1.AlluxioCompTemplateSpec{
						NetworkMode: datav1alpha1.HostNetworkMode,
					},
				},
			},
			wantValue: &Alluxio{
				Master: Master{
					HostNetwork: true,
				},
			},
		},
	}

	engine := &AlluxioEngine{Log: fake.NullLogger()}
	ds := &datav1alpha1.Dataset{}
	for k, v := range testCases {
		gotValue := &Alluxio{}
		if err := engine.transformMasters(v.runtime, ds, gotValue); err == nil {
			if gotValue.Master.HostNetwork != v.wantValue.Master.HostNetwork {
				t.Errorf("check %s failure, got:%t,want:%t",
					k,
					gotValue.Master.HostNetwork,
					v.wantValue.Master.HostNetwork,
				)
			}
		}
	}
}

func TestTransformWorkers(t *testing.T) {
	testCases := map[string]struct {
		runtime   *datav1alpha1.AlluxioRuntime
		wantValue *Alluxio
	}{
		"test network mode case 1": {
			runtime: &datav1alpha1.AlluxioRuntime{
				Spec: datav1alpha1.AlluxioRuntimeSpec{
					Worker: datav1alpha1.AlluxioCompTemplateSpec{
						NetworkMode: datav1alpha1.ContainerNetworkMode,
					},
				},
			},
			wantValue: &Alluxio{
				Worker: Worker{
					HostNetwork: false,
				},
			},
		},
		"test network mode case 2": {
			runtime: &datav1alpha1.AlluxioRuntime{
				Spec: datav1alpha1.AlluxioRuntimeSpec{
					Worker: datav1alpha1.AlluxioCompTemplateSpec{
						NetworkMode: datav1alpha1.HostNetworkMode,
					},
				},
			},
			wantValue: &Alluxio{
				Worker: Worker{
					HostNetwork: true,
				},
			},
		},
		"test network mode case 3": {
			runtime: &datav1alpha1.AlluxioRuntime{
				Spec: datav1alpha1.AlluxioRuntimeSpec{
					Worker: datav1alpha1.AlluxioCompTemplateSpec{
						NetworkMode: datav1alpha1.HostNetworkMode,
					},
				},
			},
			wantValue: &Alluxio{
				Worker: Worker{
					HostNetwork: true,
				},
			},
		},
	}

	engine := &AlluxioEngine{Log: fake.NullLogger()}
	for k, v := range testCases {
		gotValue := &Alluxio{}
		if err := engine.transformWorkers(v.runtime, gotValue); err == nil {
			if gotValue.Worker.HostNetwork != v.wantValue.Worker.HostNetwork {
				t.Errorf("check %s failure, got:%t,want:%t",
					k,
					gotValue.Worker.HostNetwork,
					v.wantValue.Worker.HostNetwork,
				)
			}
		}
	}
}

func TestGenerateStaticPorts(t *testing.T) {
	engine := &AlluxioEngine{Log: fake.NullLogger(),
		runtime: &datav1alpha1.AlluxioRuntime{
			Spec: datav1alpha1.AlluxioRuntimeSpec{
				Master: datav1alpha1.AlluxioCompTemplateSpec{
					Replicas: 3,
				},
				APIGateway: datav1alpha1.AlluxioCompTemplateSpec{
					Enabled: true,
				},
			},
		}}
	gotValue := &Alluxio{}
	engine.generateStaticPorts(gotValue)
	expect := &Alluxio{
		Master: Master{
			Ports: Ports{
				Embedded: 19200,
				Rpc:      19998,
				Web:      19999,
			},
		}, JobMaster: JobMaster{
			Ports: Ports{
				Embedded: 20003,
				Rpc:      20001,
				Web:      20002,
			},
		}, APIGateway: APIGateway{
			Ports: Ports{
				Rest: 39999,
			},
		}, Worker: Worker{
			Ports: Ports{Rpc: 29999,
				Web: 30000},
		}, JobWorker: JobWorker{
			Ports: Ports{
				Rpc:  30001,
				Data: 30002,
				Web:  30003,
			},
		},
	}

	if !reflect.DeepEqual(expect, gotValue) {
		t.Errorf("Expect the value %v, but got %v", expect, gotValue)
	}
}
