# Kora: a travel-agent persona

Kora plans trips from, to and within Indonesia, Japan, Saudi Arabia and Sweden, down to the hour: travel time, transfers, sleep, meals, how long to spend at each place, and the budget. It covers leisure, umrah and business (fixed appointments such as "in Aichi on 9 September at 10:00").

| | |
|---|---|
| Sources | 30 documents in Indonesian, about 29,500 words, compiled November 2026 (`sources/`) |
| Spec | `spec.json`: id `kora`, version `1.0.0`, retrieval budget 20 units + 3 source excerpts |
| Eval | `eval.json`: 40 held-out questions covering visas, fares, festivals, umrah, Sweden and Indonesia |

```sh
openpersona train -spec examples/travel/spec.json -src examples/travel/sources \
                  -eval examples/travel/eval.json -out packs/kora.persona -workers 8
openpersona chat -pack packs/kora.persona -q "Budget Rp 20 juta dari Jakarta, seminggu di Jepang, ikut festival musim panas"
```

## What the knowledge covers

| Files | Content |
|---|---|
| `00`–`06` | How Kora works with its tools, hour-by-hour planning rules (time zones, airport buffers, check-in times, jet lag, transfer buffers, meal times, visit durations), budget and exchange rates, the visa matrix across the four passports, health and law, climate and packing, flight corridors and airports |
| `10`–`18` | Japan: etiquette and lodging, transport (IC cards, JR Pass, shinkansen fares, airport transfers, night buses, ferries), Tokyo and day trips, Kansai, Aichi/Nagoya and Chubu, other regions, food and halal, festivals with dates, business travel |
| `20`–`22` | Saudi Arabia: basics and transport (Haramain train, Riyadh Metro), umrah (Nusuk, sequence and durations, Makkah and Madinah, daily schedule), Riyadh, Jeddah, AlUla and other regions |
| `30`–`32` | Sweden: basics, food, business; transport (Arlanda, SL, SJ, night train to Lapland, ferries); destinations |
| `40`–`44` | Indonesia: basics and food, transport (KAI, Whoosh, ASDP ferries, fast boats), Java, Bali/Lombok/Komodo, the outer islands |
| `90`–`92` | When and how to search Google; worked itineraries (Japan summer festival on Rp 20 million, business trip to Toyota City, umrah in 9 days, and four cross-country trips) |

Prices are planning estimates as of November 2026. Fast-changing facts are marked "(verifikasi)" in the sources, which tells Kora to check them with `google_search`.

## Wiring Kora into an agent with tools

Kora's product has its own actuators: `get_flights`, `get_hotels` and `google_search`. Train, bus, ferry and attraction tickets cannot be bought, so Kora's knowledge supplies the routes, fares and how to buy them. The pack does not call these tools. The agent does.

The recommended loop:

1. **System prompt.** Call `POST /v1/personas/kora/prompt` with the user's request and use the returned `system` as the agent's system prompt. It carries Kora's identity, boundaries and the knowledge for the request as a whole.
2. **Knowledge as a tool.** Give the agent a `kora_knowledge(query)` tool that calls the same endpoint with a small budget (`{"query": "...", "units": 6, "sources": -1}`) and returns `units`. A trip request has many facets (visa for this passport, this airport transfer, this festival, this city). One retrieval for the whole request has a ceiling. In a live test, a Stockholm-to-Kariya business request missed the Swedish visa rule and the Stockholm–Japan corridor even though both are in the pack. Retrieving once per facet does not have that ceiling.
3. **Live data.** The agent calls `get_flights` and `get_hotels` for prices and availability, and `google_search` only for what the knowledge marks for verification or does not have (see `90-riset-dengan-google.md`).

Example tool definitions for the agent's model:

```json
[
  {"name": "kora_knowledge", "description": "Search Kora's travel knowledge (visas, transport routes and fares, attractions, hours, food, festivals, umrah, business etiquette) for Indonesia, Japan, Saudi Arabia and Sweden. Use one focused query per sub-question.",
   "parameters": {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}},
  {"name": "get_flights", "description": "Live flights by origin, destination, date range and price range.", "parameters": {"...": "..."}},
  {"name": "get_hotels", "description": "Live hotels by area, dates, price and guests.", "parameters": {"...": "..."}},
  {"name": "google_search", "description": "Web search for facts Kora's knowledge lacks or marks as needing verification (event dates for the travel year, current fares, closures).", "parameters": {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}}
]
```

Without tools, `POST /v1/personas/kora/chat` still works in one shot. Retrieval then also runs one channel per sentence of the request, so each part of a multi-part request gets its own share of the budget.

## Updating Kora

The knowledge goes out of date, so plan to refresh it: when fares, visa rules or opening hours change, edit `sources/`, bump `version` in `spec.json`, retrain and push. A version in the registry is immutable. Before retraining, check the facts marked "(verifikasi)" again, and the festival dates for the coming year.
