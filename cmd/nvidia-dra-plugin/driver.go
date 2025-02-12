/*
 * Copyright (c) 2022-2023, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1alpha2"

	nascrd "github.com/NVIDIA/k8s-dra-driver/api/nvidia.com/resource/gpu/nas/v1alpha1"
	nasclient "github.com/NVIDIA/k8s-dra-driver/api/nvidia.com/resource/gpu/nas/v1alpha1/client"
)

const (
	CleanupTimeoutSecondsOnError = 5
)

type driver struct {
	sync.Mutex
	// TODO: Rename to nascr
	nascrd    *nascrd.NodeAllocationState
	nasclient *nasclient.Client
	state     *DeviceState
}

func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	var d *driver
	client := nasclient.New(config.nascrd, config.clientset.Nvidia.NasV1alpha1())
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := client.GetOrCreate(ctx)
		if err != nil {
			return err
		}

		err = client.UpdateStatus(ctx, nascrd.NodeAllocationStateStatusNotReady)
		if err != nil {
			return err
		}

		state, err := NewDeviceState(ctx, config)
		if err != nil {
			return err
		}

		err = client.Update(ctx, state.GetUpdatedSpec(&config.nascrd.Spec))
		if err != nil {
			return err
		}

		err = client.UpdateStatus(ctx, nascrd.NodeAllocationStateStatusReady)
		if err != nil {
			return err
		}

		d = &driver{
			nascrd:    config.nascrd,
			nasclient: client,
			state:     state,
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	go d.CleanupStaleStateContinuously(ctx)

	return d, nil
}

func (d *driver) Shutdown(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := d.nasclient.Get(ctx)
		if err != nil {
			return err
		}
		return d.nasclient.UpdateStatus(ctx, nascrd.NodeAllocationStateStatusNotReady)
	})
}

func (d *driver) NodePrepareResource(ctx context.Context, req *drapbv1.NodePrepareResourceRequest) (*drapbv1.NodePrepareResourceResponse, error) {
	d.Lock()
	defer d.Unlock()

	klog.Infof("NodePrepareResource is called: request: %+v", req)

	isPrepared, prepared, err := d.IsPrepared(ctx, req.ClaimUid)
	if err != nil {
		return nil, fmt.Errorf("error checking if claim is already prepared: %w", err)
	}

	if isPrepared {
		klog.Infof("Returning cached devices for claim '%v': %s", req.ClaimUid, prepared)
		return &drapbv1.NodePrepareResourceResponse{CdiDevices: prepared}, nil
	}

	prepared, err = d.Prepare(ctx, req.ClaimUid)
	if err != nil {
		return nil, fmt.Errorf("error preparing devices for claim %v: %w", req.ClaimUid, err)
	}

	klog.Infof("Returning newly prepared devices for claim '%v': %s", req.ClaimUid, prepared)
	return &drapbv1.NodePrepareResourceResponse{CdiDevices: prepared}, nil
}

func (d *driver) NodeUnprepareResource(ctx context.Context, req *drapbv1.NodeUnprepareResourceRequest) (*drapbv1.NodeUnprepareResourceResponse, error) {
	// We don't upprepare as part of NodeUnprepareResource, we do it
	// asynchronously when the claims themselves are deleted and the
	// AllocatedClaim has been removed.
	return &drapbv1.NodeUnprepareResourceResponse{}, nil
}

func (d *driver) IsPrepared(ctx context.Context, claimUID string) (bool, []string, error) {
	err := d.nasclient.Get(ctx)
	if err != nil {
		return false, nil, err
	}
	if _, exists := d.nascrd.Spec.PreparedClaims[claimUID]; exists {
		return true, d.state.cdi.GetClaimDevices(claimUID), nil
	}
	return false, nil, nil
}

func (d *driver) Prepare(ctx context.Context, claimUID string) ([]string, error) {
	var err error
	var prepared []string
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err = d.nasclient.Get(ctx)
		if err != nil {
			return err
		}

		prepared, err = d.state.Prepare(ctx, claimUID, d.nascrd.Spec.AllocatedClaims[claimUID])
		if err != nil {
			return err
		}

		err = d.nasclient.Update(ctx, d.state.GetUpdatedSpec(&d.nascrd.Spec))
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

func (d *driver) Unprepare(ctx context.Context, claimUID string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := d.nasclient.Get(ctx)
		if err != nil {
			return err
		}

		err = d.state.Unprepare(ctx, claimUID)
		if err != nil {
			return err
		}

		err = d.nasclient.Update(ctx, d.state.GetUpdatedSpec(&d.nascrd.Spec))
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func (d *driver) CleanupStaleStateContinuously(ctx context.Context) {
	for {
		resourceVersion, err := d.cleanupStaleStateOnce(ctx)
		if err != nil {
			klog.Errorf("Error cleaning up stale claim state: %w", err)
		}

		err = d.cleanupStaleStateContinuously(ctx, resourceVersion, err)
		if err != nil {
			klog.Errorf("Error cleaning up stale claim state: %w", err)
			time.Sleep(CleanupTimeoutSecondsOnError * time.Second)
		}
	}
}

func (d *driver) cleanupStaleStateOnce(ctx context.Context) (string, error) {
	listOptions := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", d.nascrd.Name),
	}

	list, err := d.nasclient.List(ctx, listOptions)
	if err != nil {
		return "", fmt.Errorf("error listing allocation state: %w", err)
	}

	if len(list.Items) != 1 {
		return "", fmt.Errorf("unexpected number of allocation state objects from list: %v", len(list.Items))
	}
	nas := list.Items[0]

	err = d.cleanupStaleState(ctx, &nas)
	if err != nil {
		return "", err
	}

	return list.ResourceVersion, nil
}

func (d *driver) cleanupStaleStateContinuously(ctx context.Context, resourceVersion string, previousError error) error {
	watchOptions := metav1.ListOptions{
		Watch:           true,
		ResourceVersion: resourceVersion,
		FieldSelector:   fmt.Sprintf("metadata.name=%s", d.nascrd.Name),
	}

	if previousError != nil {
		timeout := int64(CleanupTimeoutSecondsOnError)
		watchOptions.TimeoutSeconds = &timeout
	}

	watcher, err := d.nasclient.Watch(ctx, watchOptions)
	if err != nil {
		return fmt.Errorf("error setting up watch to cleanup allocations: %w", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		if event.Type != watch.Modified {
			continue
		}

		nas, ok := event.Object.(*nascrd.NodeAllocationState)
		if !ok {
			return fmt.Errorf("unexpected error decoding object from watcher")
		}

		err = d.cleanupStaleState(ctx, nas)
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *driver) cleanupStaleState(ctx context.Context, nas *nascrd.NodeAllocationState) error {
	var wg sync.WaitGroup
	var errorChans []chan error
	errorCounts := make(chan int)

	caErrors := d.cleanupClaimAllocations(ctx, nas, &wg)
	errorChans = append(errorChans, caErrors)
	go func() {
		count := 0
		for err := range caErrors {
			klog.Errorf("Error cleaning up claim allocations: %w", err)
			count++
		}
		errorCounts <- count
	}()

	cdiErrors := d.cleanupCDIFiles(nas, &wg)
	errorChans = append(errorChans, cdiErrors)
	go func() {
		count := 0
		for err := range cdiErrors {
			klog.Errorf("Error cleaning up CDI files: %w", err)
			count++
		}
		errorCounts <- count
	}()

	mpsErrors := d.cleanupMpsControlDaemonArtifacts(nas, &wg)
	errorChans = append(errorChans, mpsErrors)
	go func() {
		count := 0
		for err := range mpsErrors {
			klog.Errorf("Error cleaning up MPS control daemon artifacts: %w", err)
			count++
		}
		errorCounts <- count
	}()

	wg.Wait()
	sumErrors := 0
	for i := range errorChans {
		close(errorChans[i])
		sumErrors += <-errorCounts
	}

	if sumErrors != 0 {
		return fmt.Errorf("encountered %v errors", sumErrors)
	}

	return nil
}

func (d *driver) cleanupClaimAllocations(ctx context.Context, nas *nascrd.NodeAllocationState, wg *sync.WaitGroup) chan error {
	errors := make(chan error)
	for claimUID := range nas.Spec.PreparedClaims {
		if _, exists := nas.Spec.AllocatedClaims[claimUID]; !exists {
			wg.Add(1)
			go func(claimUID string) {
				defer wg.Done()
				klog.Infof("Attempting to unprepare resources for claim %v", claimUID)
				err := d.Unprepare(ctx, claimUID)
				if err != nil {
					errors <- fmt.Errorf("error unpreparing resources for claim %v: %w", claimUID, err)
					return
				}
				klog.Infof("Successfully unprepared resources for claim %v", claimUID)
			}(claimUID)
		}
	}
	return errors
}

func (d *driver) cleanupCDIFiles(nas *nascrd.NodeAllocationState, wg *sync.WaitGroup) chan error {
	// TODO: implement loop to remove CDI files from the CDI path for claimUIDs
	// that have been removed from the AllocatedClaims map.
	errors := make(chan error)
	return errors
}

func (d *driver) cleanupMpsControlDaemonArtifacts(nas *nascrd.NodeAllocationState, wg *sync.WaitGroup) chan error {
	// TODO: implement loop to remove mpsControlDaemon folders from the mps
	// path for claimUIDs that have been removed from the AllocatedClaims map.
	errors := make(chan error)
	return errors
}
