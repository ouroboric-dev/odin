Read docs/leaves.yaml. Write one file, out/plan/tasks.md, with exactly this content and nothing else in it:

# Tasks

## smoke-T1: the api says hello

- **Acceptance criteria:**
  - [ ] hello.txt holds the word hello
- **Named test:** the unit check
- **Layer:** repos/api
- **Size:** S
- **Depends on:** nothing

## smoke-T2: the web leaf says hello

- **Acceptance criteria:**
  - [ ] hello.txt holds the word hello
- **Named test:** the unit check
- **Layer:** repos/web
- **Size:** S
- **Depends on:** smoke-T1

Do nothing else: no other files, no commands that change anything else. Then reply with the word done.
