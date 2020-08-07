package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	v1 "github.com/mrajashree/backup/pkg/apis/backupper.cattle.io/v1"
	util "github.com/mrajashree/backup/pkg/controllers"
	backupControllers "github.com/mrajashree/backup/pkg/generated/controllers/backupper.cattle.io/v1"
	"github.com/rancher/wrangler/pkg/condition"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	//k8scorev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	k8sv1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	//"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/storage/value"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"os"
	"path/filepath"
	"time"
)

type handler struct {
	ctx                     context.Context
	backups                 backupControllers.BackupController
	backupTemplates         backupControllers.BackupTemplateController
	backupEncryptionConfigs backupControllers.BackupEncryptionConfigController
	discoveryClient         discovery.DiscoveryInterface
	dynamicClient           dynamic.Interface
}

func Register(
	ctx context.Context,
	backups backupControllers.BackupController,
	backupTemplates backupControllers.BackupTemplateController,
	backupEncryptionConfigs backupControllers.BackupEncryptionConfigController,
	clientSet *clientset.Clientset,
	dynamicInterface dynamic.Interface) {

	controller := &handler{
		ctx:                     ctx,
		backups:                 backups,
		backupTemplates:         backupTemplates,
		backupEncryptionConfigs: backupEncryptionConfigs,
		discoveryClient:         clientSet.Discovery(),
		dynamicClient:           dynamicInterface,
	}

	// Register handlers
	backups.OnChange(ctx, "backups", controller.OnBackupChange)
}

func (h *handler) OnBackupChange(_ string, backup *v1.Backup) (*v1.Backup, error) {
	if backup.DeletionTimestamp != nil || backup == nil {
		return backup, nil
	}
	if condition.Cond(v1.BackupConditionReady).IsTrue(backup) && condition.Cond(v1.BackupConditionUploaded).IsTrue(backup) {
		return backup, nil
	}
	// empty dir defaults to os.TempDir
	tmpBackupPath, err := ioutil.TempDir("", backup.Spec.BackupFileName)
	if err != nil {
		return backup, fmt.Errorf("error creating temp dir: %v", err)
	}
	logrus.Infof("Temporary backup path is %v", tmpBackupPath)
	//h.discoveryClient.ServerGroupsAndResources()
	transformerMap, err := h.getEncryptionTransformers(backup)
	if err != nil {
		removeDirErr := os.RemoveAll(tmpBackupPath)
		if removeDirErr != nil {
			return backup, errors.New(err.Error() + removeDirErr.Error())
		}
		return backup, err
	}

	template, err := h.backupTemplates.Get(backup.Namespace, backup.Spec.BackupTemplate, k8sv1.GetOptions{})
	if err != nil {
		removeDirErr := os.RemoveAll(tmpBackupPath)
		if removeDirErr != nil {
			return backup, errors.New(err.Error() + removeDirErr.Error())
		}
		return backup, err
	}
	resourceCollectionStartTime := time.Now()
	logrus.Infof("Started gathering resources at %v", resourceCollectionStartTime)
	rh := util.ResourceHandler{
		DiscoveryClient: h.discoveryClient,
		DynamicClient:   h.dynamicClient,
	}
	resourcesWithStatusSubresource, err := rh.GatherResources(h.ctx, template.BackupFilters, tmpBackupPath, transformerMap)
	//err = h.gatherResources(template.BackupFilters, tmpBackupPath, transformerMap)
	if err != nil {
		removeDirErr := os.RemoveAll(tmpBackupPath)
		if removeDirErr != nil {
			return backup, errors.New(err.Error() + removeDirErr.Error())
		}
		return backup, err
	}
	timeTakenToCollectResources := time.Since(resourceCollectionStartTime)
	logrus.Infof("time taken to collect resources: %v", timeTakenToCollectResources)
	filters, err := json.Marshal(template.BackupFilters)
	if err != nil {
		removeDirErr := os.RemoveAll(tmpBackupPath)
		if removeDirErr != nil {
			return backup, errors.New(err.Error() + removeDirErr.Error())
		}
		return backup, err
	}
	filtersPath := filepath.Join(tmpBackupPath, "filters")
	err = os.Mkdir(filtersPath, os.ModePerm)
	if err != nil {
		removeDirErr := os.RemoveAll(tmpBackupPath)
		if removeDirErr != nil {
			return backup, errors.New(err.Error() + removeDirErr.Error())
		}
	}

	err = ioutil.WriteFile(filepath.Join(filtersPath, "filters.json"), filters, os.ModePerm)
	if err != nil {
		removeDirErr := os.RemoveAll(tmpBackupPath)
		if removeDirErr != nil {
			return backup, errors.New(err.Error() + removeDirErr.Error())
		}
		return backup, err
	}

	subresources, err := json.Marshal(resourcesWithStatusSubresource)
	if err != nil {
		panic(err)
		removeDirErr := os.RemoveAll(tmpBackupPath)
		if removeDirErr != nil {
			return backup, errors.New(err.Error() + removeDirErr.Error())
		}
		return backup, err

	}
	err = ioutil.WriteFile(filepath.Join(filtersPath, "statussubresource.json"), subresources, os.ModePerm)
	if err != nil {
		removeDirErr := os.RemoveAll(tmpBackupPath)
		if removeDirErr != nil {
			return backup, errors.New(err.Error() + removeDirErr.Error())
		}
		return backup, err
	}

	condition.Cond(v1.BackupConditionReady).SetStatusBool(backup, true)
	gzipFile := backup.Spec.BackupFileName + ".tar.gz"
	if backup.Spec.Local != "" {
		// for local, to send backup tar to given local path, use that as the path when creating compressed file
		if err := util.CreateTarAndGzip(tmpBackupPath, backup.Spec.Local, gzipFile); err != nil {
			removeDirErr := os.RemoveAll(tmpBackupPath)
			if removeDirErr != nil {
				return backup, errors.New(err.Error() + removeDirErr.Error())
			}
			return backup, err
		}
	} else if backup.Spec.ObjectStore != nil {
		if err := h.uploadToS3(backup, tmpBackupPath, gzipFile); err != nil {
			removeDirErr := os.RemoveAll(tmpBackupPath)
			if removeDirErr != nil {
				return backup, errors.New(err.Error() + removeDirErr.Error())
			}
			return backup, err
		}
	}
	condition.Cond(v1.BackupConditionUploaded).SetStatusBool(backup, true)
	if err := os.RemoveAll(tmpBackupPath); err != nil {
		return backup, err
	}
	if updBackup, err := h.backups.UpdateStatus(backup); err != nil {
		return updBackup, err
	}
	logrus.Infof("Done with backup")

	return backup, err
}

func (h *handler) getEncryptionTransformers(backup *v1.Backup) (map[schema.GroupResource]value.Transformer, error) {
	var transformerMap map[schema.GroupResource]value.Transformer
	// EncryptionConfig secret ns is hardcoded to ns of controller in chart's ns
	// TODO: change secret ns to the chart's ns
	config, err := h.backupEncryptionConfigs.Get("default", backup.Spec.EncryptionConfigName, k8sv1.GetOptions{})
	if err != nil {
		return transformerMap, err
	}
	return util.GetEncryptionTransformers(config)
}
