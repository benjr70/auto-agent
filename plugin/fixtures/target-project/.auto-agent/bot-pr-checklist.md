## Manual verification

Bot-PR checklist (fixture). Run against the hermetic environment only.

- [ ] `python3 -m unittest discover -s app` still passes after the bump
- [ ] `/api/health` answers on a fresh `up`
