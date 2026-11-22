output "state_bucket" {
  description = "Repository variable TF_STATE_BUCKET, and -backend-config=bucket=… for local init."
  value       = aws_s3_bucket.state.bucket
}

output "github_actions_role_arn" {
  description = "Repository variable AWS_ROLE_ARN."
  value       = aws_iam_role.github_actions.arn
}
