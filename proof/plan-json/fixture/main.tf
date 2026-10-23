terraform {
  required_version = "= 1.13.2"
}

variable "stage" {
  type = number
}

variable "confidential" {
  type      = string
  sensitive = true
}

resource "terraform_data" "app" {
  input = {
    revision = tostring(var.stage)
    public   = "visible"
    nested   = { private = var.confidential }
    entries  = [{ public = "visible-item", private = var.confidential }]
  }
}

resource "terraform_data" "replace_destroy_first" {
  triggers_replace = var.stage
}

resource "terraform_data" "replace_create_first" {
  triggers_replace = var.stage
  lifecycle {
    create_before_destroy = true
  }
}

resource "terraform_data" "delete_me" {
  count = var.stage == 1 ? 1 : 0
  input = "first-stage"
}

resource "terraform_data" "create_me" {
  count = var.stage == 2 ? 1 : 0
  input = "second-stage"
}

resource "terraform_data" "protected" {
  input = var.stage
}

resource "terraform_data" "protected_neighbor" {
  input = "unchanged"
}

output "sensitive_result" {
  value     = var.confidential
  sensitive = true
}

output "computed_id" {
  value = terraform_data.app.id
}
