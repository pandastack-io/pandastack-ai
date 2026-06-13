terraform {
  backend "gcs" {
    # Set your own Terraform state bucket here (create it once with
    # `gsutil mb gs://<your-tfstate-bucket>`), or override at init time:
    #   terraform init -backend-config="bucket=<your-tfstate-bucket>"
    bucket = "REPLACE_WITH_YOUR_TFSTATE_BUCKET"
    prefix = "pandastack-ai"
  }
}
