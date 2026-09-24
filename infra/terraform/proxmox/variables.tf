# Written by `bin/auto-agent setup --provision proxmox` into a tfvars file on
# the Operator machine; none of these is a secret.

variable "proxmox_endpoint" {
  description = "Proxmox VE API URL, e.g. https://pve.example:8006/"
  type        = string
}

variable "proxmox_insecure" {
  description = "Skip TLS verification (a self-signed Proxmox certificate)"
  type        = bool
  default     = false
}

variable "name" {
  description = "The Host's name: the VM name and its inventory entry"
  type        = string
}

variable "node" {
  description = "Proxmox node the VM runs on"
  type        = string
}

variable "vm_id" {
  description = "Optional static VMID (Proxmox picks the next free one otherwise)"
  type        = number
  default     = null
}

variable "datastore" {
  description = "Datastore for the VM disk and its cloud-init drive"
  type        = string
  default     = "local-lvm"
}

variable "image_datastore" {
  description = "Datastore (with the ISO content type) the cloud image is downloaded to"
  type        = string
  default     = "local"
}

variable "image_url" {
  description = "Ubuntu 24.04 Server cloud image (ADR 0004); the arm64 one on an arm64 node"
  type        = string
  default     = "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
}

variable "bridge" {
  description = "Network bridge"
  type        = string
  default     = "vmbr0"
}

variable "vlan_tag" {
  description = "Optional VLAN tag (0 for none)"
  type        = number
  default     = 0
}

variable "ipv4_cidr" {
  description = "The VM's static IPv4 address in CIDR form; Setup reaches it there over SSH"
  type        = string
}

variable "gateway" {
  description = "IPv4 gateway"
  type        = string
}

variable "dns_servers" {
  description = "DNS servers (the gateway's resolver when empty)"
  type        = list(string)
  default     = []
}

variable "cores" {
  description = "CPU cores"
  type        = number
  default     = 4
}

# ADR 0004: the Daemon peaks at 6.4 GB RSS, plus swap and per-PR images.
variable "memory_mb" {
  description = "Memory in MB"
  type        = number
  default     = 12288
}

variable "disk_gb" {
  description = "Boot disk in GB"
  type        = number
  default     = 80
}

variable "vm_user" {
  description = "The Host user cloud-init creates (passwordless sudo, key-only login)"
  type        = string
  default     = "auto-agent"
}

variable "ssh_public_keys" {
  description = "The operator's SSH public keys"
  type        = list(string)

  validation {
    condition     = length(var.ssh_public_keys) > 0
    error_message = "At least one SSH public key: it is the only way into the VM."
  }
}

variable "tags" {
  description = "Proxmox tags"
  type        = list(string)
  default     = ["auto-agent"]
}
