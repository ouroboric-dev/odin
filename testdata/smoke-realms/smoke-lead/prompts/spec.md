Write one file, out/leaves.yaml, with exactly this content and nothing else in it:

leaves:
  repos/api:
    realm: smoke-api
    realm_version: 0.1.0
    description: The smoke test's api.
  repos/web:
    realm: smoke-lead
    realm_version: 0.1.0
    description: The smoke test's web leaf.
    consumes: [repos/api]

Do nothing else: no other files, no commands that change anything else. Then reply with the word done.
