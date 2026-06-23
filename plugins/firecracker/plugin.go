package firecracker

import "voidrun/pkg/compute"

func init() {
	compute.Register(string(compute.TypeFirecracker), func() compute.Hypervisor {
		return &Provider{}
	})
}

type Provider struct{}

func (p *Provider) Name() string { return string(compute.TypeFirecracker) }
