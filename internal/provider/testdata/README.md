`sigv4_botocore.json` holds Authorization headers produced by botocore for
Bedrock Converse requests, with the timestamp botocore used. `sigv4_test.go`
requires the hand-written Go signer to reproduce them exactly.

Regenerate (needs `pip install botocore`, in a throwaway virtualenv):

    python3 gen_sigv4_golden.py > sigv4_botocore.json
