# Normalization criteria — moved

The criteria that used to live here (R1–R3, N1–N3, B1–B6, P1–P3) are
folded, with the other two doctrines the estate carried, into one
**component contract** in `truvity/policy`:

**<https://github.com/truvity/policy/blob/master/docs/contracts/component.md>**

Its rules carry the IDs `C1`–`C13`; cite those, not the old codes. This
repository no longer carries doctrine of its own.

One rule changed on the way: **N3's `0.0.0-dev` is retired.** Every
committed `Chart.yaml` now carries `version: 0.0.0` (and
`appVersion: 0.0.0`), and the release stamps both from the tag. That is
contract rule C1, and this repository's charts follow it.
