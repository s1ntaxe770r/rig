package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/rigdev/rig-go-api/api/v1/capsule"
	"github.com/rigdev/rig-go-api/model"
	"github.com/rigdev/rig/gen/go/registry"
	"github.com/rigdev/rig/internal/gateway/cluster"
	"github.com/rigdev/rig/pkg/api/v1alpha1"
	"github.com/rigdev/rig/pkg/errors"
	"github.com/rigdev/rig/pkg/iterator"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
)

func (c *Client) CreateCapsuleConfig(ctx context.Context, cfg *v1alpha1.Capsule) error {
	if err := c.rcc.CreateCapsuleConfig(ctx, cfg); err != nil {
		return err
	}

	return c.applyCapsuleConfig(ctx, cfg.Name)
}

func (c *Client) UpdateCapsuleConfig(ctx context.Context, cfg *v1alpha1.Capsule) error {
	if err := c.rcc.UpdateCapsuleConfig(ctx, cfg); err != nil {
		return err
	}

	return c.applyCapsuleConfig(ctx, cfg.Name)
}

func (c *Client) ListCapsuleConfigs(ctx context.Context, pagination *model.Pagination) (iterator.Iterator[*v1alpha1.Capsule], int64, error) {
	return c.rcc.ListCapsuleConfigs(ctx, pagination)
}

func (c *Client) applyCapsuleConfig(ctx context.Context, capsuleID string) error {
	c.logger.Debug("creating docker capsule", zap.String("capsuleID", capsuleID))

	cfg, err := c.rcc.GetCapsuleConfig(ctx, capsuleID)
	if err != nil {
		return err
	}

	envs, err := c.rcc.GetEnvironmentVariables(ctx, capsuleID)
	if err != nil {
		return err
	}

	if cfg.Spec.Image == "" {
		return nil
	}

	var regAuth *cluster.RegistryAuth
	if cfg.Spec.ImagePullSecret != nil {
		s, err := c.GetSecret(ctx, capsuleID, cfg.Spec.ImagePullSecret.Name, cfg.Namespace)
		if err != nil {
			return err
		}

		var out struct {
			Auths map[string]struct {
				Auth string
			}
		}
		if err := json.Unmarshal(s.Data[".dockerconfigjson"], &out); err != nil {
			return err
		}

		for host, a := range out.Auths {
			auth, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return err
			}

			parts := strings.SplitN(string(auth), ":", 2)
			if len(parts) != 2 {
				return errors.InvalidArgumentErrorf("invalid .dockerconfigjson auth")
			}

			regAuth = &cluster.RegistryAuth{
				Host: host,
				RegistrySecret: &registry.Secret{
					Username: parts[0],
					Password: parts[1],
				},
			}
		}
	}

	network := &capsule.Network{}
	for _, i := range cfg.Spec.Interfaces {
		netIf := &capsule.Interface{
			Name:   i.Name,
			Port:   uint32(i.Port),
			Public: &capsule.PublicInterface{},
		}

		if i.Public != nil {
			netIf.Public.Enabled = true
			switch {
			case i.Public.Ingress != nil:
				netIf.Public.Method = &capsule.RoutingMethod{
					Kind: &capsule.RoutingMethod_Ingress_{
						Ingress: &capsule.RoutingMethod_Ingress{
							Host: i.Public.Ingress.Host,
						},
					},
				}
			case i.Public.LoadBalancer != nil:
				netIf.Public.Method = &capsule.RoutingMethod{
					Kind: &capsule.RoutingMethod_LoadBalancer_{
						LoadBalancer: &capsule.RoutingMethod_LoadBalancer{
							Port: uint32(i.Public.LoadBalancer.Port),
						},
					},
				}
			}
		}

		network.Interfaces = append(network.Interfaces, netIf)
	}

	var cf []*capsule.ConfigFile
	for _, f := range cfg.Spec.Files {
		if f.ConfigMap != nil {
			cm, err := c.GetFile(ctx, capsuleID, f.ConfigMap.Name, cfg.Namespace)
			if err != nil {
				return err
			}

			cf = append(cf, &capsule.ConfigFile{
				Path:    f.Path,
				Content: cm.BinaryData[f.ConfigMap.Key],
			})
		}
	}

	replicas := int32(1)
	if cfg.Spec.Replicas != nil {
		replicas = *cfg.Spec.Replicas
	}

	return c.upsertCapsule(ctx, cfg.GetName(), &cluster.Capsule{
		CapsuleID: cfg.GetName(),
		Image:     cfg.Spec.Image,
		ContainerSettings: &capsule.ContainerSettings{
			EnvironmentVariables: envs,
		},
		Network:      network,
		Replicas:     uint32(replicas),
		Namespace:    cfg.GetNamespace(),
		RegistryAuth: regAuth,
		ConfigFiles:  cf,
	})
}

func (c *Client) GetCapsuleConfig(ctx context.Context, capsuleID string) (*v1alpha1.Capsule, error) {
	return c.rcc.GetCapsuleConfig(ctx, capsuleID)
}

func (c *Client) DeleteCapsuleConfig(ctx context.Context, capsuleID string) error {
	if err := c.deleteCapsule(ctx, capsuleID); err != nil {
		return err
	}

	return c.rcc.DeleteCapsuleConfig(ctx, capsuleID)
}

func (c *Client) SetEnvironmentVariables(ctx context.Context, capsuleID string, envs map[string]string) error {
	if err := c.rcc.SetEnvironmentVariables(ctx, capsuleID, envs); err != nil {
		return err
	}

	return c.applyCapsuleConfig(ctx, capsuleID)
}

func (c *Client) GetEnvironmentVariables(ctx context.Context, capsuleID string) (map[string]string, error) {
	return c.rcc.GetEnvironmentVariables(ctx, capsuleID)
}

func (c *Client) SetEnvironmentVariable(ctx context.Context, capsuleID, name, value string) error {
	envs, err := c.rcc.GetEnvironmentVariables(ctx, capsuleID)
	if err != nil {
		return err
	}

	envs[name] = value

	if err := c.rcc.SetEnvironmentVariables(ctx, capsuleID, envs); err != nil {
		return err
	}

	return nil
}

func (c *Client) GetEnvironmentVariable(ctx context.Context, capsuleID, name string) (value string, ok bool, err error) {
	envs, err := c.rcc.GetEnvironmentVariables(ctx, capsuleID)
	if err != nil {
		return "", false, err
	}

	if v, ok := envs[name]; ok {
		return v, ok, nil
	}

	return "", false, nil
}

func (c *Client) DeleteEnvironmentVariable(ctx context.Context, capsuleID, name string) error {
	envs, err := c.rcc.GetEnvironmentVariables(ctx, capsuleID)
	if err != nil {
		return err
	}

	delete(envs, name)

	if err := c.rcc.SetEnvironmentVariables(ctx, capsuleID, envs); err != nil {
		return err
	}

	return nil
}

func (c *Client) GetFile(ctx context.Context, capsuleID, name, namespace string) (*v1.ConfigMap, error) {
	fs, err := c.rcc.GetFiles(ctx, capsuleID)
	if err != nil {
		return nil, err
	}

	for _, f := range fs {
		if f.Name == name && f.Namespace == namespace {
			return f, nil
		}
	}

	return nil, errors.NotFoundErrorf("file not found")
}

func (c *Client) SetFile(ctx context.Context, capsuleID string, file *v1.ConfigMap) error {
	fs, err := c.rcc.GetFiles(ctx, capsuleID)
	if err != nil {
		return err
	}

	found := false
	for i, f := range fs {
		if f.Name == file.Name && f.Namespace == file.Namespace {
			fs[i] = file
			found = true
			break
		}
	}
	if !found {
		fs = append(fs, file)
	}

	if err := c.rcc.SetFiles(ctx, capsuleID, fs); err != nil {
		return err
	}

	return nil
}

func (c *Client) ListFiles(ctx context.Context, capsuleID string, pagination *model.Pagination) (iterator.Iterator[*v1.ConfigMap], int64, error) {
	fs, err := c.rcc.GetFiles(ctx, capsuleID)
	if err != nil {
		return nil, 0, err
	}

	return iterator.FromList(fs), int64(len(fs)), nil
}

func (c *Client) DeleteFile(ctx context.Context, capsuleID, name, namespace string) error {
	fs, err := c.rcc.GetFiles(ctx, capsuleID)
	if err != nil {
		return err
	}

	for i, f := range fs {
		if f.Name == name && f.Namespace == namespace {
			fs = append(fs[:i], fs[i+1:]...)
			break
		}
	}

	if err := c.rcc.SetFiles(ctx, capsuleID, fs); err != nil {
		return err
	}

	return nil
}

func (c *Client) GetSecret(ctx context.Context, capsuleID, name, namespace string) (*v1.Secret, error) {
	ss, err := c.rcc.GetSecrets(ctx, capsuleID)
	if err != nil {
		return nil, err
	}

	for _, s := range ss {
		if s.Name == name && s.Namespace == namespace {
			return s, nil
		}
	}

	return nil, errors.NotFoundErrorf("secret not found")
}

func (c *Client) SetSecret(ctx context.Context, capsuleID string, secret *v1.Secret) error {
	ss, err := c.rcc.GetSecrets(ctx, capsuleID)
	if err != nil {
		return err
	}

	found := false
	for i, s := range ss {
		if s.Name == secret.Name && s.Namespace == secret.Namespace {
			ss[i] = secret
			found = true
			break
		}
	}
	if !found {
		ss = append(ss, secret)
	}

	if err := c.rcc.SetSecrets(ctx, capsuleID, ss); err != nil {
		return err
	}

	return nil
}

func (c *Client) ListSecrets(ctx context.Context, capsuleID string, pagination *model.Pagination) (iterator.Iterator[*v1.Secret], int64, error) {
	ss, err := c.rcc.GetSecrets(ctx, capsuleID)
	if err != nil {
		return nil, 0, err
	}

	return iterator.FromList(ss), int64(len(ss)), nil
}

func (c *Client) DeleteSecret(ctx context.Context, capsuleID, name, namespace string) error {
	ss, err := c.rcc.GetSecrets(ctx, capsuleID)
	if err != nil {
		return err
	}

	for i, s := range ss {
		if s.Name == name && s.Namespace == namespace {
			ss = append(ss[:i], ss[i+1:]...)
			break
		}
	}

	if err := c.rcc.SetSecrets(ctx, capsuleID, ss); err != nil {
		return err
	}

	return nil
}
