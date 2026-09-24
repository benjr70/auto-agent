# The API token is the one secret this environment needs, and it never becomes
# a variable: the provider reads it from PROXMOX_VE_API_TOKEN
# (user@realm!tokenid=secret), which Setup exports into terraform's
# environment only. Provider configuration is not persisted to state, and no
# variable here is a secret, so the state file is free of secrets (Spec #23).
provider "proxmox" {
  endpoint = var.proxmox_endpoint
  insecure = var.proxmox_insecure
}
