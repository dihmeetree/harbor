packer {
  required_plugins {
    hcloud = {
      source  = "github.com/hetznercloud/hcloud"
      version = "~> 1.4.0"
    }
  }
}

variable "channel" {
  type    = string
  default = "beta"
}

variable "hcloud_token" {
  type      = string
  default   = env("HCLOUD_TOKEN")
  sensitive = true
}

source "hcloud" "flatcar" {
  token = var.hcloud_token

  image    = "ubuntu-24.04"
  location = "fsn1"
  rescue   = "linux64"

  snapshot_labels = {
    os      = "flatcar"
    channel = var.channel
  }

  ssh_username = "root"
}

build {
  source "hcloud.flatcar" {
    name          = "x86"
    server_type   = "cx22"
    snapshot_name = "flatcar-${var.channel}-x86"
  }

  source "hcloud.flatcar" {
    name          = "arm"
    server_type   = "cax11"
    snapshot_name = "flatcar-${var.channel}-arm"
  }

  provisioner "shell" {
    inline = [
      # Download script and dependencies
      "apt-get -y install gawk",
      "curl -fsSLO --retry-delay 1 --retry 60 --retry-connrefused --retry-max-time 60 --connect-timeout 20 https://raw.githubusercontent.com/flatcar/init/flatcar-master/bin/flatcar-install",
      "chmod +x flatcar-install",

      # Install flatcar
      "./flatcar-install -s -o hetzner -C ${var.channel}",
    ]
  }
}