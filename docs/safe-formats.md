# Safe formats

Some model files can run code when a client loads them. PyTorch checkpoints, `joblib` dumps and anything else built on Python's `pickle` execute whatever the author put in them at the moment `torch.load` or `pickle.load` reads the file. The `safetensors` format cannot: it is a JSON header followed by raw tensors.

The **safe formats** setting makes the proxy serve only the second kind. It is off by default.

## Turning it on

- In the console: **Settings → Cache rules → Safe formats**.
- Or with `HFPROXY_SAFE_FORMATS_ONLY=true`. Like the other console settings it can be locked to the environment with `HFPROXY_LOCKED_SETTINGS=safe_formats_only`.

The change applies at once, without a restart.

## What is refused

By name, case-insensitively, whatever the folder:

| Extension | Why |
| --- | --- |
| `.bin`, `.pt`, `.pth`, `.ckpt` | PyTorch and legacy checkpoints (pickle inside a zip) |
| `.pkl`, `.pickle`, `.pck` | Plain pickles |
| `.joblib` | scikit-learn style dumps |
| `.pdparams` | PaddlePaddle parameters |

`.bin` also catches legacy ggml files, because the name does not tell them from a PyTorch `.bin`. `.npy` and `.npz` stay allowed: NumPy does not unpickle them unless the caller asks for it. `.h5` and `.keras` stay allowed too, although a Keras model can carry a Lambda layer; the proxy does not inspect them.

## What happens

| Where | Result |
| --- | --- |
| A client reads a file (`resolve`, GET or HEAD) | `404` with `X-Error-Code: EntryNotFound` and a message saying why. The Hub is not asked and nothing is cached. |
| Preload | The file is skipped, not counted as a failure. The job reports how many were skipped. |
| Publishing a file (preupload, commit, console upload) | `422` with the reason, before anything is recorded. |
| Publishing a file named `.safetensors` | Its header is checked: an 8-byte length, then a JSON object that fits in the file. A pickle renamed to `.safetensors` gets a `422`. |
| A file already cached or published before the setting was turned on | No longer served. It stays in storage until it is evicted, or deleted by its owner. |

A refused read is a `404` rather than a `403` on purpose. `transformers` prefers `model.safetensors` and only falls back to `pytorch_model.bin` when it is missing, so a repository that ships both keeps working. A repository that ships only a pickle becomes unusable, with a message that says why.

## Exempting a repository

Some models only exist as pickles. An administrator can allow them one repository at a time, on the **Repositories** page (the *Allow pickle-based files* box, then **Save**) or with the API:

```bash
curl -X PATCH https://hfproxy.example.com/api/v1/repos/REPOSITORY_ID \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"allow_unsafe_formats": true}'
```

The change is recorded in the audit log (`repository.unsafe_formats`). An exempted repository is also exempt from the header check.

## Limits

- It looks at names and, for `.safetensors`, at the header of published files. It does not scan the content of other files, and it does not check `.safetensors` files cached from the Hub (only the ones published here).
- `trust_remote_code` loads Python files from a repository. The setting does not block `.py` files.
- `hfproxy_unsafe_format_refused_total{where}` counts the refusals (`read`, `preload`, `publish`).
