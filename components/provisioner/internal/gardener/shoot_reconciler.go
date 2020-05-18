package gardener

import (
	"context"
	"encoding/json"
	"os"

	"github.com/kyma-incubator/compass/components/provisioner/internal/installation"

	"github.com/kyma-incubator/compass/components/provisioner/internal/persistence/dberrors"

	"github.com/kyma-incubator/compass/components/provisioner/internal/director"

	"k8s.io/client-go/util/retry"

	"github.com/kyma-incubator/compass/components/provisioner/internal/provisioning/persistence/dbsession"

	"github.com/gardener/gardener/pkg/client/core/clientset/versioned/typed/core/v1beta1"

	gardener_types "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NewReconciler(
	mgr ctrl.Manager,
	dbsFactory dbsession.Factory,
	shootClient v1beta1.ShootInterface,
	directorClient director.DirectorClient,
	installationSvc installation.Service,
	auditLogTenantConfigPath string) *Reconciler {
	return &Reconciler{
		client:     mgr.GetClient(),
		scheme:     mgr.GetScheme(),
		log:        logrus.WithField("Component", "ShootReconciler"),
		dbsFactory: dbsFactory,

		auditLogTenantConfigPath: auditLogTenantConfigPath,

		provisioningOperator: &ProvisioningOperator{
			dbsFactory:      dbsFactory,
			shootClient:     shootClient,
			directorClient:  directorClient,
			installationSvc: installationSvc,
		},
	}
}

type Reconciler struct {
	client     client.Client
	scheme     *runtime.Scheme
	dbsFactory dbsession.Factory

	log                  *logrus.Entry
	provisioningOperator *ProvisioningOperator

	auditLogTenantConfigPath string
}

type ProvisioningOperator struct {
	shootClient     v1beta1.ShootInterface
	dbsFactory      dbsession.Factory
	directorClient  director.DirectorClient
	installationSvc installation.Service
}

func (r *Reconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithField("Shoot", req.NamespacedName)
	log.Infof("reconciling Shoot")

	var shoot gardener_types.Shoot
	if err := r.client.Get(context.Background(), req.NamespacedName, &shoot); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		log.Error(err, "unable to get shoot")
		return ctrl.Result{}, err
	}

	shouldReconcile, err := r.shouldReconcileShoot(shoot)
	if err != nil {
		log.Errorf("Failed to verify if shoot should be reconciled: %s", err.Error())
		return ctrl.Result{}, err
	}
	if !shouldReconcile {
		log.Debugf("Gardener cluster not found in database, shoot will be ignored")
		return ctrl.Result{}, nil
	}
	runtimeId := getRuntimeId(shoot)
	log = log.WithField("RuntimeId", runtimeId)

	seed := getSeed(shoot)
	if seed != "" && r.auditLogTenantConfigPath != "" {
		err := r.enableAuditLogs(log, &shoot, seed)
		if err != nil {
			log.Errorf("Failed to enable audit logs for %s shoot: %s", shoot.Name, err.Error())
		}
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) handleShootWithoutOperationId(log *logrus.Entry, shoot gardener_types.Shoot) error {
	// TODO: We can verify shoot status here - ensure it is ok
	log.Debug("Shoot without operation ID is ignored for now")
	return nil
}

func (r *Reconciler) shouldReconcileShoot(shoot gardener_types.Shoot) (bool, error) {
	session := r.dbsFactory.NewReadSession()

	_, err := session.GetGardenerClusterByName(shoot.Name)
	if err != nil {
		if err.Code() == dberrors.CodeNotFound {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func (r *ProvisioningOperator) updateShoot(shoot gardener_types.Shoot, modifyShootFn func(s *gardener_types.Shoot)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		refetchedShoot, err := r.shootClient.Get(shoot.Name, v1.GetOptions{})
		if err != nil {
			return err
		}

		modifyShootFn(refetchedShoot)

		refetchedShoot, err = r.shootClient.Update(refetchedShoot)
		if err != nil {
			return err
		}

		return nil
	})
}

func (r *Reconciler) enableAuditLogs(logger logrus.FieldLogger, shoot *gardener_types.Shoot, seed string) error {
	logger.Info("Enabling audit logs")
	tenant, err := r.getAuditLogTenant(seed)
	if err != nil {
		return err
	}

	if tenant == "" {
		logger.Warnf("Cannot enable audit logs. Tenant for seed %s is empty", seed)
		return nil
	} else if tenant == shoot.Annotations[auditLogsAnnotation] {
		logger.Debugf("Seed for cluster did not change, skipping annotating with Audit Log Tenant")
		return nil
	}

	logger.Infof("Modifying Audit Log Tenant")

	return r.provisioningOperator.updateShoot(*shoot, func(s *gardener_types.Shoot) {
		annotate(s, auditLogsAnnotation, tenant)
	})
}

func (r *Reconciler) getAuditLogTenant(seed string) (string, error) {
	file, err := os.Open(r.auditLogTenantConfigPath)

	if err != nil {
		return "", err
	}

	defer file.Close()

	var data map[string]string
	if err := json.NewDecoder(file).Decode(&data); err != nil {
		return "", err
	}
	return data[seed], nil
}

func getSeed(shoot gardener_types.Shoot) string {
	if shoot.Spec.SeedName != nil {
		return *shoot.Spec.SeedName
	}

	return ""
}
