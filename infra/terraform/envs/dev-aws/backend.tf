terraform {
  backend "s3" {
    # Set your own Terraform state bucket here (create it once with
    # `aws s3 mb s3://<your-tfstate-bucket>`), or override at init time:
    #   terraform init -backend-config="bucket=<your-tfstate-bucket>"
    bucket = "REPLACE_WITH_YOUR_TFSTATE_BUCKET"
    key    = "pandastack-ai/dev-aws.tfstate"
    region = "us-east-1"
    # Optional state locking via DynamoDB:
    #   dynamodb_table = "<your-tflock-table>"
    encrypt = true
  }
}
