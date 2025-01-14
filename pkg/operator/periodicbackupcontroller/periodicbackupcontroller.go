package periodicbackupcontroller

import (
	"context"
	"fmt"
	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	operatorv1 "github.com/openshift/api/operator/v1"
	backupv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	"k8s.io/klog/v2"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/client-go/kubernetes"
)

type PeriodicBackupController struct {
	operatorClient        v1helpers.OperatorClient
	backupsClient         backupv1client.BackupsGetter
	kubeClient            kubernetes.Interface
	operatorImagePullSpec string
	featureGateAccessor   featuregates.FeatureGateAccess
}

func NewPeriodicBackupController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.OperatorClient,
	backupsClient backupv1client.BackupsGetter,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	operatorImagePullSpec string,
	accessor featuregates.FeatureGateAccess,
	backupsInformer factory.Informer) factory.Controller {

	c := &PeriodicBackupController{
		operatorClient:        operatorClient,
		backupsClient:         backupsClient,
		kubeClient:            kubeClient,
		operatorImagePullSpec: operatorImagePullSpec,
		featureGateAccessor:   accessor,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("PeriodicBackupController", syncer)

	return factory.New().
		ResyncEvery(1*time.Minute).
		WithInformers(backupsInformer).
		WithSync(syncer.Sync).
		ToController("PeriodicBackupController", eventRecorder.WithComponentSuffix("periodic-backup-controller"))
}

func (c *PeriodicBackupController) sync(ctx context.Context, _ factory.SyncContext) error {
	if enabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); !enabled {
		if err != nil {
			klog.V(4).Infof("PeriodicBackupController error while checking feature flags: %v", err)
		}
		return nil
	}

	cronJobsClient := c.kubeClient.BatchV1().CronJobs(operatorclient.TargetNamespace)
	backups, err := c.backupsClient.Backups().List(ctx, v1.ListOptions{})
	if err != nil {
		return fmt.Errorf("PeriodicBackupController could not list backup CRDs, error was: %w", err)
	}

	for _, item := range backups.Items {
		err := reconcileCronJob(ctx, cronJobsClient, item, c.operatorImagePullSpec)
		if err != nil {
			_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:    "PeriodicBackupControllerDegraded",
				Status:  operatorv1.ConditionTrue,
				Reason:  "Error",
				Message: err.Error(),
			}))
			if updateErr != nil {
				klog.V(4).Infof("PeriodicBackupController error during UpdateStatus: %v", err)
			}

			return fmt.Errorf("PeriodicBackupController could not reconcile backup [%s] with cronjob: %w", item.Name, err)
		}
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "PeriodicBackupControllerDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "AsExpected",
	}))
	if updateErr != nil {
		klog.V(4).Infof("PeriodicBackupController error during UpdateStatus: %v", err)
	}

	return nil
}

func reconcileCronJob(ctx context.Context,
	cronJobClient batchv1client.CronJobInterface,
	backup backupv1alpha1.Backup,
	operatorImagePullSpec string) error {

	create := false
	currentCronJob, err := cronJobClient.Get(ctx, backup.Name, v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			create = true
		} else {
			return fmt.Errorf("PeriodicBackupController could not get cronjob %s: %w", backup.Name, err)
		}
	}

	cronJob, err := newCronJob()
	if err != nil {
		return err
	}

	cronJob.Name = backup.Name
	cronJob.OwnerReferences = append(cronJob.OwnerReferences, v1.OwnerReference{
		APIVersion: backup.APIVersion,
		Kind:       backup.Kind,
		Name:       backup.Name,
		UID:        backup.UID,
	})

	cronJob.Spec.Schedule = backup.Spec.EtcdBackupSpec.Schedule
	if backup.Spec.EtcdBackupSpec.TimeZone != "" {
		cronJob.Spec.TimeZone = &backup.Spec.EtcdBackupSpec.TimeZone
	}

	if len(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers) == 0 {
		cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers = append(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers, corev1.Container{})
	}

	cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image = operatorImagePullSpec
	cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Args = []string{
		"--backup-name=" + backup.Name,
		"--retention-number=5",
		"--pvc=" + backup.Spec.EtcdBackupSpec.PVCName,
	}

	if create {
		_, err := cronJobClient.Create(ctx, cronJob, v1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("PeriodicBackupController could not create cronjob %s: %w", cronJob.Name, err)
		}
	} else {
		if cronSpecDiffers(cronJob.Spec, currentCronJob.Spec) {
			klog.V(4).Infof("detected diff in cronjob specs, updating cronjob: %s", cronJob.Name)
			_, err := cronJobClient.Update(ctx, cronJob, v1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("PeriodicBackupController could not update cronjob %s: %w", cronJob.Name, err)
			}
		}
	}

	return nil
}

func cronSpecDiffers(l batchv1.CronJobSpec, r batchv1.CronJobSpec) bool {
	lBytes, _ := l.Marshal()
	rBytes, _ := r.Marshal()

	if len(lBytes) != len(rBytes) {
		return true
	}

	for i := 0; i < len(lBytes); i++ {
		if lBytes[i] != rBytes[i] {
			return true
		}
	}

	return false
}

func newCronJob() (*batchv1.CronJob, error) {
	scheme := runtime.NewScheme()
	codec := serializer.NewCodecFactory(scheme)
	err := batchv1.AddToScheme(scheme)
	if err != nil {
		return nil, fmt.Errorf("PeriodicBackupController could not add batchv1 scheme: %w", err)
	}

	obj, err := runtime.Decode(codec.UniversalDecoder(batchv1.SchemeGroupVersion), etcd_assets.MustAsset("etcd/cluster-backup-cronjob.yaml"))
	if err != nil {
		return nil, fmt.Errorf("PeriodicBackupController could not decode batchv1 job scheme: %w", err)
	}

	return obj.(*batchv1.CronJob), nil
}
