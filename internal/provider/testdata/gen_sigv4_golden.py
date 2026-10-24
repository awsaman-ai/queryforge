# Generates reference SigV4 signatures with botocore for the Go golden tests.
import json, datetime
from botocore.auth import SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.credentials import Credentials
from unittest import mock

cases = []
def run(name, model, token, body):
    # botocore percent-encodes the model id in the path exactly as the SDK would.
    from urllib.parse import quote
    host = "bedrock-runtime.us-east-1.amazonaws.com"
    url = f"https://{host}/model/{quote(model, safe='')}/converse"
    req = AWSRequest(method="POST", url=url, data=body, headers={"Content-Type": "application/json"})
    creds = Credentials("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", token)
    fixed = datetime.datetime(2026, 10, 24, 12, 30, 45)
    with mock.patch("botocore.auth.datetime") as dt:
        dt.datetime.utcnow.return_value = fixed
        dt.datetime.now.return_value = fixed
        dt.datetime.strptime = datetime.datetime.strptime
        SigV4Auth(creds, "bedrock", "us-east-1").add_auth(req)
    cases.append({"name": name, "model": model, "token": token or "", "body": body,
                  "authorization": req.headers["Authorization"], "amzdate": req.headers["X-Amz-Date"]})

body = '{"messages":[{"role":"user","content":[{"text":"hi"}]}]}'
run("plain id", "anthropic.claude-3-5-sonnet-20241022-v2:0", None, body)
run("profile id", "us.anthropic.claude-sonnet-4-6", None, body)
run("session token", "anthropic.claude-3-5-sonnet-20241022-v2:0", "FQoGZXIvYXdzEXAMPLETOKEN+/=", body)
run("arn", "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-4-6", None, body)
print(json.dumps(cases, indent=1))
