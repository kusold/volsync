/*
Copyright 2021 The VolSync authors.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package syncthing

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/backube/volsync/api/v1alpha1"
	"github.com/backube/volsync/controllers"
	"github.com/backube/volsync/controllers/mover"
	"github.com/backube/volsync/controllers/utils"
)

// constants used in the syncthing configuration
const (
	dataDirEnv             = "SYNCTHING_DATA_DIR"
	dataDirMountPath       = "/data"
	configDirEnv           = "SYNCTHING_CONFIG_DIR"
	configDirMountPath     = "/config"
	configPVCName          = "syncthing-config"
	syncthingJobName       = "syncthing"
	syncthingContainerName = "syncthing"
	syncthingAPIPort       = 8384
	syncthingDataPort      = 22000
	appLabelName           = "syncthing"
	apiKeySecretName       = "syncthing-apikey"
	apiServiceName         = "syncthing-api"
	dataServiceName        = "syncthing-data"
)

// Mover is the reconciliation logic for the Restic-based data mover.
type Mover struct {
	client      client.Client
	logger      logr.Logger
	owner       metav1.Object
	isSource    bool
	paused      bool
	dataPVCName *string
	peerList    []v1alpha1.SyncthingPeer
	status      *v1alpha1.ReplicationSourceSyncthingStatus
	apiKey      string // store the API key in here to avoid repeated calls
}

var _ mover.Mover = &Mover{}

// All object types that are temporary/per-iteration should be listed here. The
// individual objects to be cleaned up must also be marked.
var cleanupTypes = []client.Object{}

func (m *Mover) Name() string { return "syncthing" }

// We need the following resources available to us in the cluster:
// - PVC for syncthing-config
// - PVC that needs to be synced
// - Secret for the syncthing-apikey
// - Job/Pod running the syncthing mover image
// - Service exposing the syncthing REST API for us to make requests to
func (m *Mover) Synchronize(ctx context.Context) (mover.Result, error) {
	var err error
	// ensure the data pvc exists
	if _, err = m.ensureDataPVC(ctx); err != nil {
		return mover.InProgress(), err
	}

	// create PVC for config data
	if _, err = m.ensureConfigPVC(ctx); err != nil {
		return mover.InProgress(), err
	}

	// ensure the secret exists
	if _, err = m.ensureSecretAPIKey(ctx); err != nil {
		return mover.InProgress(), err
	}

	// ensure the job exists
	if _, err = m.ensureJob(ctx); err != nil {
		return mover.InProgress(), err
	}

	// create the service for the syncthing REST API
	if _, err = m.ensureService(ctx); err != nil {
		return mover.InProgress(), err
	}

	// ensure the external service exists
	if _, err = m.ensureDataService(ctx); err != nil {
		return mover.InProgress(), err
	}

	if _, err = m.ensureIsConfigured(ctx); err != nil {
		return mover.InProgress(), err
	}

	if err = m.ensureStatusIsUpdated(ctx); err != nil {
		m.logger.V(3).Error(err, "could not update mover status")
		return mover.InProgress(), err
	}

	var retryAfter = 20 * time.Second
	return mover.RetryAfter(retryAfter), nil
}

func (m *Mover) ensureConfigPVC(ctx context.Context) (*corev1.PersistentVolumeClaim, error) {
	configPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configPVCName,
			Namespace: m.owner.GetNamespace(),
		},
	}
	if err := m.client.Get(ctx, client.ObjectKeyFromObject(configPVC), configPVC); err == nil {
		// pvc already exists
		m.logger.Info("PVC already exists:  " + configPVC.Name)
		return configPVC, nil
	}

	// otherwise, create the PVC
	configPVC = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configPVCName,
			Namespace: m.owner.GetNamespace(),
			Labels: map[string]string{
				"app": appLabelName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	if err := m.client.Create(ctx, configPVC); err != nil {
		return nil, err
	}
	m.logger.Info("Created PVC", configPVC.Name, configPVC)
	return configPVC, nil
}

func (m *Mover) ensureDataPVC(ctx context.Context) (*corev1.PersistentVolumeClaim, error) {
	// check if the data PVC exists, error if it doesn't
	fmt.Printf("Checking for PVC %s\n", *m.dataPVCName)
	dataPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      *m.dataPVCName,
			Namespace: m.owner.GetNamespace(),
			Labels: map[string]string{
				"app": appLabelName,
			},
		},
	}
	if err := m.client.Get(ctx, client.ObjectKeyFromObject(dataPVC), dataPVC); err != nil {
		// pvc doesn't exist
		return nil, err
	}
	return dataPVC, nil
}

func (m *Mover) ensureSecretAPIKey(ctx context.Context) (*corev1.Secret, error) {
	/*
		The secret is in the following format:
		apiVersion: v1
		kind: Secret
		metadata:
			name: st-apikey
		type: Opaque
		data:
			apiKey: 'cGFzc3dvcmQxMjM='

	*/
	// check if the secret exists, error if it doesn't
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiKeySecretName,
			Namespace: m.owner.GetNamespace(),
			Labels: map[string]string{
				"app": appLabelName,
			},
		},
	}
	err := m.client.Get(ctx, client.ObjectKeyFromObject(secret), secret)

	if err != nil {
		// need to create the secret
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      apiKeySecretName,
				Namespace: m.owner.GetNamespace(),
				Labels: map[string]string{
					"app": appLabelName,
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				// base64 encode an empty string
				"apikey": []byte("password123"),
			},
		}
		if err := m.client.Create(ctx, secret); err != nil {
			// error creating secret
			m.logger.Error(err, "Error creating secret")
			return nil, err
		}
		m.logger.Info("Created secret", secret.Name, secret)
	}
	return secret, nil
}

//nolint:funlen
func (m *Mover) ensureJob(ctx context.Context) (*batchv1.Job, error) {
	// return successfully if the job exists, try to create it otherwise
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      syncthingJobName,
			Namespace: m.owner.GetNamespace(),
			Labels: map[string]string{
				"app": appLabelName,
			},
		},
	}
	err := m.client.Get(ctx, client.ObjectKeyFromObject(job), job)
	if err == nil {
		// job already exists
		return job, nil
	}
	if !errors.IsNotFound(err) {
		// something about the job is broken
		m.logger.Error(err, "Error getting job")
		return nil, err
	}

	var ttlSecondsAfterFinished int32 = 100
	var configVolumeName, dataVolumeName string = "syncthing-config", "syncthing-data"

	// job doesn't exist, create it
	job = &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      syncthingJobName,
			Namespace: m.owner.GetNamespace(),
			Labels: map[string]string{
				"app": appLabelName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttlSecondsAfterFinished,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  syncthingContainerName,
							Image: syncthingContainerImage,
							Command: []string{
								"/entry.sh",
							},
							Args: []string{
								"run",
							},
							Env: []corev1.EnvVar{
								{
									Name:  configDirEnv,
									Value: configDirMountPath,
								},
								{
									Name:  dataDirEnv,
									Value: dataDirMountPath,
								},
								{
									Name: "STGUIAPIKEY",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: apiKeySecretName,
											},
											Key: "apikey",
										},
									},
								},
							},
							ImagePullPolicy: corev1.PullAlways,
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: syncthingAPIPort,
								},
								{
									ContainerPort: syncthingDataPort,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      configVolumeName,
									MountPath: configDirMountPath,
								},
								{
									Name:      dataVolumeName,
									MountPath: dataDirMountPath,
								},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: configVolumeName,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: configPVCName,
								},
							},
						},
						{
							Name: dataVolumeName,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: *m.dataPVCName,
								},
							},
						},
					},
				},
			},
		},
	}

	// pass the object onto the k8s api
	err = m.client.Create(ctx, job)
	return job, err
}

func (m *Mover) ensureService(ctx context.Context) (*corev1.Service, error) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiServiceName,
			Namespace: m.owner.GetNamespace(),
			Labels: map[string]string{
				"app": appLabelName,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": appLabelName,
			},
			Ports: []corev1.ServicePort{
				{
					Port:       syncthingAPIPort,
					TargetPort: intstr.FromInt(syncthingAPIPort),
					Protocol:   "TCP",
				},
			},
		},
	}
	err := m.client.Get(ctx, client.ObjectKeyFromObject(service), service)
	if err == nil {
		// service already exists
		m.logger.Info("service already exists", "service", service.Name)
		return service, nil
	}

	if err := m.client.Create(ctx, service); err != nil {
		m.logger.Error(err, "error creating the service")
		return nil, err
	}
	return service, nil
}

func (m *Mover) ensureDataService(ctx context.Context) (*corev1.Service, error) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataServiceName,
			Namespace: m.owner.GetNamespace(),
			Labels: map[string]string{
				"app": appLabelName,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": appLabelName,
			},
			Ports: []corev1.ServicePort{
				{
					Port:       syncthingDataPort,
					TargetPort: intstr.FromInt(syncthingDataPort),
					Protocol:   "TCP",
				},
			},
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}
	err := m.client.Get(ctx, client.ObjectKeyFromObject(service), service)
	if err == nil {
		m.logger.Info("service already exists", "service", service.Name)
		if service.Status.LoadBalancer.Ingress != nil && len(service.Status.LoadBalancer.Ingress) > 0 {
			m.status.Address = "tcp://" + service.Status.LoadBalancer.Ingress[0].IP + ":" + strconv.Itoa(syncthingDataPort)
		}
		return service, nil
	}

	if err := m.client.Create(ctx, service); err != nil {
		m.logger.Error(err, "error creating the service")
		return nil, err
	}
	if service.Status.LoadBalancer.Ingress != nil && len(service.Status.LoadBalancer.Ingress) > 0 {
		m.status.Address = "tcp://" + service.Status.LoadBalancer.Ingress[0].IP + ":" + strconv.Itoa(syncthingDataPort)
	}
	return service, nil
}

func (m *Mover) Cleanup(ctx context.Context) (mover.Result, error) {
	err := utils.CleanupObjects(ctx, m.client, m.logger, m.owner, cleanupTypes)
	if err != nil {
		return mover.InProgress(), err
	}
	return mover.Complete(), nil
}

// get the API key
func (m *Mover) getAPIKey(ctx context.Context) (string, error) {
	// get the syncthing-apikey secret
	if m.apiKey == "" {
		secret := &corev1.Secret{}
		err := m.client.Get(ctx, client.ObjectKey{Name: apiKeySecretName, Namespace: m.owner.GetNamespace()}, secret)
		if err != nil {
			return "", err
		}
		m.apiKey = string(secret.Data["apikey"])
	}
	return m.apiKey, nil
}

func (m *Mover) getSyncthingRequestHeaders(ctx context.Context) (map[string]string, error) {
	// get the API key from the syncthing-apikey secret
	var apiKey string
	var err error
	if apiKey, err = m.getAPIKey(ctx); err != nil {
		return nil, err
	}
	headers := map[string]string{
		"X-API-Key":    apiKey,
		"Content-Type": "application/json",
	}
	return headers, nil
}

func (m *Mover) getSyncthingConfig(ctx context.Context) (*SyncthingConfig, error) {
	headers, err := m.getSyncthingRequestHeaders(ctx)
	if err != nil {
		return nil, err
	}
	responseBody := &SyncthingConfig{
		Devices: []SyncthingDevice{},
		Folders: []SyncthingFolder{},
	}
	data, err := controllers.JSONRequest("https://127.0.0.1:8384/rest/config", "GET", headers, nil)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(data, responseBody)
	// print out what we got back from golang
	// m.logger.Info("Response from syncthing REST API", "response", responseBody)
	return responseBody, err
}

func (m *Mover) getSyncthingSystemStatus(ctx context.Context) (*SystemStatus, error) {
	headers, err := m.getSyncthingRequestHeaders(ctx)
	if err != nil {
		return nil, err
	}
	responseBody := &SystemStatus{}
	data, err := controllers.JSONRequest("https://127.0.0.1:8384/rest/system/status", "GET", headers, nil)
	if err != nil {
		return nil, err
	}
	// unmarshal the data into the responseBody
	err = json.Unmarshal(data, responseBody)
	// m.logger.Info("Response from syncthing REST API", "response", responseBody)
	return responseBody, err
}

func (m *Mover) updateSyncthingConfig(ctx context.Context, config *SyncthingConfig) (*SyncthingConfig, error) {
	headers, err := m.getSyncthingRequestHeaders(ctx)
	if err != nil {
		return nil, err
	}
	// we only want to update the folders and devices
	responseBody := &SyncthingConfig{
		Devices: []SyncthingDevice{},
		Folders: []SyncthingFolder{},
	}
	data, err := controllers.JSONRequest("https://127.0.0.1:8384/rest/config", "PUT", headers, config)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(data, responseBody)
	return responseBody, err
}

func (m *Mover) ensureIsConfigured(ctx context.Context) (mover.Result, error) {
	config, err := m.getSyncthingConfig(ctx)
	if err != nil {
		return mover.InProgress(), err
	}
	m.logger.V(4).Info("Syncthing config", "config", config)
	status, err := m.getSyncthingSystemStatus(ctx)
	if err != nil {
		m.logger.Error(err, "error getting syncthing system status")
		return mover.InProgress(), err
	}

	// check if the syncthing is configured
	if NeedsReconfigure(config.Devices, m.peerList, status.MyID) {
		m.logger.Info("Syncthing needs reconfiguration")

		// update settings
		config.Devices = UpdateDevices(m, config, status)
		config.Folders = UpdateFolders(config)

		m.logger.V(4).Info("Updated Syncthing config for update", "config", config)
		if config, err = m.updateSyncthingConfig(ctx, config); err != nil {
			m.logger.Error(err, "error updating syncthing config")
			return mover.InProgress(), err
		}
		m.logger.V(4).Info("Syncthing config after configuration", "config", config)
	}

	return mover.Complete(), nil
}

func (m *Mover) getConnectedStatus(ctx context.Context) (*SystemConnections, error) {
	headers, err := m.getSyncthingRequestHeaders(ctx)
	if err != nil {
		return nil, err
	}
	responseBody := &SystemConnections{}
	data, err := controllers.JSONRequest("https://127.0.0.1:8384/rest/system/connections", "GET", headers, nil)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(data, responseBody)
	return responseBody, err
}

func (m *Mover) ensureStatusIsUpdated(ctx context.Context) error {
	// get the current status
	status, err := m.getSyncthingSystemStatus(ctx)
	if err != nil {
		return err
	}

	connected, err := m.getConnectedStatus(ctx)

	m.status.DeviceID = status.MyID
	m.status.Peers = []v1alpha1.SyncthingPeerStatus{}

	// add the connected devices to the status
	for _, device := range m.peerList {
		if (device.ID == status.MyID) || (device.ID == "") {
			continue
		}

		// check connection status
		devStats, ok := connected.Connections[device.ID]
		m.status.Peers = append(m.status.Peers, v1alpha1.SyncthingPeerStatus{
			ID:        device.ID,
			Address:   device.Address,
			Connected: ok && devStats.Connected,
		})
	}
	return err
}
