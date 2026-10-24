Your answer is one ygg.plan/v1 document, with exactly this content and nothing else in it:

{
  "title": "Smoke",
  "slug": "smoke",
  "goal": "Each leaf says hello.",
  "decisions": [],
  "risks": [],
  "tasks": [
    {
      "id": "smoke-T001",
      "title": "the api says hello",
      "criteria": [
        "hello.txt holds the word hello",
        "the unit check passes"
      ],
      "named_test": "the unit check",
      "layer": "repos/api",
      "size": "S",
      "depends_on": [],
      "text": "Write hello.txt with the word hello in it."
    },
    {
      "id": "smoke-T002",
      "title": "the web leaf says hello",
      "criteria": [
        "hello.txt holds the word hello",
        "the unit check passes"
      ],
      "named_test": "the unit check",
      "layer": "repos/web",
      "size": "S",
      "depends_on": [
        "smoke-T001"
      ],
      "text": "Write hello.txt with the word hello in it."
    }
  ]
}

Do nothing else: no files, no commands that change anything. Hand the document back as the section on your answer says.
