package decide

// realResponse is a 200 answer captured from the OpenRouter Decisions API.
const realResponse = `{
  "id": "gen-dec-1790015143-AIaTutprXsJ5EwohRSjb",
  "model": "typesafe/jev-1.13-20260917",
  "provider": "TypeSafe",
  "answers": {
    "is_bug": { "type": "noul", "noul": 0.96 },
    "team": { "type": "choice", "choice": "payments", "confidence": 0.67,
      "probabilities": { "payments": 0.78, "frontend": 0.22, "account": 0 } }
  },
  "usage": { "input_tokens": 476, "output_tokens": 70, "cost": 0.000019992 }
}`
