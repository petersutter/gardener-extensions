// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package genericactuator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gardener/gardener-extensions/pkg/controller"
	"github.com/gardener/gardener-extensions/pkg/util"

	gardencorev1alpha1helper "github.com/gardener/gardener/pkg/apis/core/v1alpha1/helper"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	machinev1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	forceDeletionLabelKey   = "force-deletion"
	forceDeletionLabelValue = "True"
)

func (a *genericActuator) Delete(ctx context.Context, worker *extensionsv1alpha1.Worker, cluster *controller.Cluster) error {
	workerDelegate, err := a.delegateFactory.WorkerDelegate(ctx, worker, cluster)
	if err != nil {
		return errors.Wrapf(err, "could not instantiate actuator context")
	}

	// Make sure machine-controller-manager is awake before deleting the machines.
	deployment := &appsv1.Deployment{}
	if err := a.client.Get(ctx, kutil.Key(worker.Namespace, a.mcmName), deployment); err != nil {
		return err
	}
	if err := util.ScaleDeployment(ctx, a.client, deployment, 1); err != nil {
		return err
	}

	// Make sure that all RBAC roles required by the machine-controller-manager exist in the shoot cluster.
	// This code can be removed as soon as the RBAC roles are managed by the gardener-resource-manager.
	if err := a.applyMachineControllerManagerShootChart(ctx, workerDelegate, worker, cluster); err != nil {
		return errors.Wrapf(err, "could not apply machine-controller-manager shoot chart")
	}

	// Mark all existing machines to become forcefully deleted.
	a.logger.Info("Deleting all machines", "worker", fmt.Sprintf("%s/%s", worker.Namespace, worker.Name))
	if err := a.markAllMachinesForcefulDeletion(ctx, worker.Namespace); err != nil {
		return errors.Wrapf(err, "marking all machines for forceful deletion failed")
	}

	// Get the list of all existing machine deployments.
	existingMachineDeployments := &machinev1alpha1.MachineDeploymentList{}
	if err := a.client.List(ctx, client.InNamespace(worker.Namespace), existingMachineDeployments); err != nil {
		return err
	}

	// Delete all machine deployments.
	if err := a.cleanupMachineDeployments(ctx, existingMachineDeployments, nil); err != nil {
		return errors.Wrapf(err, "cleaning up machine deployments failed")
	}

	// Delete all machine classes.
	if err := a.cleanupMachineClasses(ctx, worker.Namespace, workerDelegate.MachineClassList(), nil); err != nil {
		return errors.Wrapf(err, "cleaning up machine classes failed")
	}

	// Delete all machine class secrets.
	if err := a.cleanupMachineClassSecrets(ctx, worker.Namespace, nil); err != nil {
		return errors.Wrapf(err, "cleaning up machine class secrets failed")
	}

	// Wait until all machine resources have been properly deleted.
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	if err := a.waitUntilMachineResourcesDeleted(timeoutCtx, worker, workerDelegate); err != nil {
		return gardencorev1alpha1helper.DetermineError(fmt.Sprintf("Failed while waiting for all machine resources to be deleted: '%s'", err.Error()))
	}

	// Delete the machine-controller-manager.
	a.logger.Info("Deleting the machine-controller-manager", "worker", fmt.Sprintf("%s/%s", worker.Namespace, worker.Name))
	shootClients, err := util.NewClientsForShoot(ctx, a.client, worker.Namespace, client.Options{})
	if err != nil {
		return errors.Wrapf(err, "could not create shoot client for cleanup of machine-controller-manager resources")
	}
	if err := a.mcmShootChart.Delete(ctx, shootClients.Client(), metav1.NamespaceSystem); err != nil {
		return errors.Wrapf(err, "cleaning up machine-controller-manager resources in shoot failed")
	}
	if err := a.mcmSeedChart.Delete(ctx, a.client, worker.Namespace); err != nil {
		return errors.Wrapf(err, "cleaning up machine-controller-manager resources in seed failed")
	}

	return nil
}

// Mark all existing machines to become forcefully deleted.
func (a *genericActuator) markAllMachinesForcefulDeletion(ctx context.Context, namespace string) error {
	// Mark all existing machines to become forcefully deleted.
	existingMachines := &machinev1alpha1.MachineList{}
	if err := a.client.List(ctx, &client.ListOptions{Namespace: namespace}, existingMachines); err != nil {
		return err
	}

	var (
		errorList []error
		wg        sync.WaitGroup
	)

	// TODO: Use github.com/gardener/gardener/pkg/utils/flow.Parallel as soon as we can vendor a new Gardener version again.
	for _, machine := range existingMachines.Items {
		wg.Add(1)
		go func(m machinev1alpha1.Machine) {
			defer wg.Done()
			if err := a.markMachineForcefulDeletion(ctx, &m); err != nil {
				errorList = append(errorList, err)
			}
		}(machine)
	}

	wg.Wait()
	if len(errorList) > 0 {
		return fmt.Errorf("labelling machines (to become forcefully deleted) failed: %v", errorList)
	}

	return nil
}

// markMachineForcefulDeletion labels a machine object to become forcefully deleted.
func (a *genericActuator) markMachineForcefulDeletion(ctx context.Context, machine *machinev1alpha1.Machine) error {
	if machine.Labels == nil {
		machine.Labels = map[string]string{}
	}

	if val, ok := machine.Labels[forceDeletionLabelKey]; ok && val == forceDeletionLabelValue {
		return nil
	}

	machine.Labels[forceDeletionLabelKey] = forceDeletionLabelValue
	return a.client.Update(ctx, machine)
}

// waitUntilMachineResourcesDeleted waits for a maximum of 30 minutes until all machine resources have been properly
// deleted by the machine-controller-manager. It polls the status every 5 seconds.
// TODO: Parallelise this?
func (a *genericActuator) waitUntilMachineResourcesDeleted(ctx context.Context, worker *extensionsv1alpha1.Worker, workerDelegate WorkerDelegate) error {
	var (
		countMachines            = -1
		countMachineSets         = -1
		countMachineDeployments  = -1
		countMachineClasses      = -1
		countMachineClassSecrets = -1
	)

	return wait.PollUntil(5*time.Second, func() (bool, error) {
		msg := ""

		// Check whether all machines have been deleted.
		if countMachines != 0 {
			existingMachines := &machinev1alpha1.MachineList{}
			if err := a.client.List(ctx, client.InNamespace(worker.Namespace), existingMachines); err != nil {
				return false, err
			}
			countMachines = len(existingMachines.Items)
			msg += fmt.Sprintf("%d machines, ", countMachines)
		}

		// Check whether all machine sets have been deleted.
		if countMachineSets != 0 {
			existingMachineSets := &machinev1alpha1.MachineSetList{}
			if err := a.client.List(ctx, client.InNamespace(worker.Namespace), existingMachineSets); err != nil {
				return false, err
			}
			countMachineSets = len(existingMachineSets.Items)
			msg += fmt.Sprintf("%d machine sets, ", countMachineSets)
		}

		// Check whether all machine deployments have been deleted.
		if countMachineDeployments != 0 {
			existingMachineDeployments := &machinev1alpha1.MachineDeploymentList{}
			if err := a.client.List(ctx, client.InNamespace(worker.Namespace), existingMachineDeployments); err != nil {
				return false, err
			}
			countMachineDeployments = len(existingMachineDeployments.Items)
			msg += fmt.Sprintf("%d machine deployments, ", countMachineDeployments)

			// Check whether an operation failed during the deletion process.
			for _, existingMachineDeployment := range existingMachineDeployments.Items {
				for _, failedMachine := range existingMachineDeployment.Status.FailedMachines {
					return false, fmt.Errorf("Machine %s failed: %s", failedMachine.Name, failedMachine.LastOperation.Description)
				}
			}
		}

		// Check whether all machine classes have been deleted.
		if countMachineClasses != 0 {
			machineClassList := workerDelegate.MachineClassList()
			if err := a.client.List(ctx, client.InNamespace(worker.Namespace), machineClassList); err != nil {
				return false, err
			}
			machineClasses, err := meta.ExtractList(machineClassList)
			if err != nil {
				return false, err
			}
			countMachineClasses = len(machineClasses)
			msg += fmt.Sprintf("%d machine classes, ", countMachineClasses)
		}

		// Check whether all machine class secrets have been deleted.
		if countMachineClassSecrets != 0 {
			count := 0
			existingMachineClassSecrets, err := a.listMachineClassSecrets(ctx, worker.Namespace)
			if err != nil {
				return false, err
			}
			for _, machineClassSecret := range existingMachineClassSecrets.Items {
				if len(machineClassSecret.Finalizers) != 0 {
					count++
				}
			}
			countMachineClassSecrets = count
			msg += fmt.Sprintf("%d machine class secrets, ", countMachineClassSecrets)
		}

		if countMachines != 0 || countMachineSets != 0 || countMachineDeployments != 0 || countMachineClasses != 0 || countMachineClassSecrets != 0 {
			a.logger.Info(fmt.Sprintf("Waiting until the following machine resources have been processed: %s", strings.TrimSuffix(msg, ", ")), "worker", fmt.Sprintf("%s/%s", worker.Namespace, worker.Name))
			return false, nil
		}
		return true, nil
	}, ctx.Done())
}
