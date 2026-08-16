package airgap

import (
	"fmt"
	"path/filepath"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

const (
	ModeOnline  = "online"
	ModeOffline = "offline"

	RegistryPort   = 5000
	FileServerPort = 8888

	RegistryContainer   = "dpubnkctl-registry"
	FileServerContainer = "dpubnkctl-fileserver"
	RegistryImage       = "registry:2"
	FileServerImage     = "nginx:stable-alpine"

	StagingDir   = "artifacts/airgap"
	CertSubDir   = "certs"
	ImagesSubDir = "images"
	DPUImgSubDir = "images-dpu"
	FilesSubDir  = "files"
	ChartsSubDir = "charts"
	FSSubDir     = "fileserver"
	DPUDebSubDir = "dpu-debs"
)

type Config struct {
	Mode         string
	JumphostIP   string
	RegistryHost string
	FilesRepo    string
	StagingDir   string
	CertDir      string
}

func NewConfig(repoDir string, p *poc.PoC) *Config {
	if p.Airgap == nil {
		return nil
	}
	staging := filepath.Join(repoDir, StagingDir)
	ip := p.Airgap.JumphostIP
	return &Config{
		Mode:         p.Airgap.Mode,
		JumphostIP:   ip,
		RegistryHost: fmt.Sprintf("%s:%d", ip, RegistryPort),
		FilesRepo:    fmt.Sprintf("http://%s:%d", ip, FileServerPort),
		StagingDir:   staging,
		CertDir:      filepath.Join(staging, CertSubDir),
	}
}
