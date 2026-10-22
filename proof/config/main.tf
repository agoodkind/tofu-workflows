terraform {
  required_version = "= 1.13.2"
}

variable "private_fixture_value" {
  type      = string
  sensitive = true
  default   = "synthetic-value-for-redaction-check"
}

resource "terraform_data" "release" {
  input = "release-one"
}

resource "terraform_data" "private_value" {
  input = var.private_fixture_value
}

output "observed_release" {
  value = terraform_data.release.output
}
