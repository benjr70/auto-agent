terraform {
  required_version = ">= 1.5.0"

  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "~> 0.114.0"
    }
  }

  # State lives on the Operator machine, one file per Host beside its
  # inventory entry: `setup --provision proxmox` passes
  # -backend-config=path=<inventory>/<name>.proxmox.tfstate. It holds no
  # secret (see providers.tf).
  backend "local" {}
}
