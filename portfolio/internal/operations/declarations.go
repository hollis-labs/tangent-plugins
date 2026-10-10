// Domain declarations ported from tracker 5ff4d2e. No source files, operator
// data, transport, credentials or runtime dependency are bundled.
package operations

const declarationsJSON = `[
  {
    "name": "databases",
    "description": "List the databases with their id prefix, item count and last-updated date.",
    "input": {
      "type": "object",
      "required": [],
      "properties": {}
    },
    "write": false
  },
  {
    "name": "list",
    "description": "List items of a database. filters match top-level fields exactly (array fields match membership). Default order: by \u0060order\u0060, then file order. sort: field name, '-' prefix for descending.",
    "input": {
      "type": "object",
      "required": [
        "db"
      ],
      "properties": {
        "db": {
          "type": "string"
        },
        "filters": {
          "type": "object"
        },
        "sort": {
          "type": "string"
        }
      }
    },
    "write": false
  },
  {
    "name": "get",
    "description": "Get one item by id.",
    "input": {
      "type": "object",
      "required": [
        "db",
        "id"
      ],
      "properties": {
        "db": {
          "type": "string"
        },
        "id": {
          "type": "string"
        }
      }
    },
    "write": false
  },
  {
    "name": "create",
    "description": "Create an item. id is generated when missing (sequential for priorities/decisions/risks/inbox, slug of the title for workstreams/roadmap/ideas). rev=1, created/updated, author and order (end of list) are defaulted.",
    "input": {
      "type": "object",
      "required": [
        "db",
        "item"
      ],
      "properties": {
        "db": {
          "type": "string"
        },
        "item": {
          "type": "object"
        }
      }
    },
    "write": true
  },
  {
    "name": "update",
    "description": "Merge \u0060patch\u0060 into an item (top-level fields; arrays are replaced; null removes a field). id, created, comments and rev cannot be patched. If \u0060rev\u0060 is given and differs from the item's current rev, fails with a conflict that carries the current item.",
    "input": {
      "type": "object",
      "required": [
        "db",
        "id",
        "patch"
      ],
      "properties": {
        "db": {
          "type": "string"
        },
        "id": {
          "type": "string"
        },
        "patch": {
          "type": "object"
        },
        "rev": {
          "type": "integer"
        }
      }
    },
    "write": true
  },
  {
    "name": "comment",
    "description": "Append a comment {id c-<n>, author, text, created, kind?} to an item. Returns the updated item.",
    "input": {
      "type": "object",
      "required": [
        "db",
        "id",
        "text"
      ],
      "properties": {
        "db": {
          "type": "string"
        },
        "id": {
          "type": "string"
        },
        "text": {
          "type": "string"
        },
        "author": {
          "type": "string"
        },
        "kind": {
          "enum": [
            "comment",
            "decision"
          ]
        }
      }
    },
    "write": true
  },
  {
    "name": "link",
    "description": "Add to.id to from's \u0060field\u0060 (default related_ids). Both ends must exist. Idempotent. Returns the \u0060from\u0060 item.",
    "input": {
      "type": "object",
      "required": [
        "from",
        "to"
      ],
      "properties": {
        "from": {
          "type": "object",
          "required": [
            "db",
            "id"
          ],
          "properties": {
            "db": {
              "type": "string"
            },
            "id": {
              "type": "string"
            }
          }
        },
        "to": {
          "type": "object",
          "required": [
            "db",
            "id"
          ],
          "properties": {
            "db": {
              "type": "string"
            },
            "id": {
              "type": "string"
            }
          }
        },
        "field": {
          "type": "string"
        }
      }
    },
    "write": true
  },
  {
    "name": "unlink",
    "description": "Remove to.id from from's \u0060field\u0060 (default related_ids). Both ends must exist. Idempotent. Returns the \u0060from\u0060 item.",
    "input": {
      "type": "object",
      "required": [
        "from",
        "to"
      ],
      "properties": {
        "from": {
          "type": "object",
          "required": [
            "db",
            "id"
          ],
          "properties": {
            "db": {
              "type": "string"
            },
            "id": {
              "type": "string"
            }
          }
        },
        "to": {
          "type": "object",
          "required": [
            "db",
            "id"
          ],
          "properties": {
            "db": {
              "type": "string"
            },
            "id": {
              "type": "string"
            }
          }
        },
        "field": {
          "type": "string"
        }
      }
    },
    "write": true
  },
  {
    "name": "link_add",
    "description": "Add an external pointer {kind, ref, label?} to an item's \u0060links\u0060 array (kind is the schema enum: torque, tesseract, tether, pr, adr, url, file, commit, workstream; ref is a non-empty string; a url must be http(s), a torque ref a task id; a file ref is a plain pointer and is never read). A duplicate kind+ref is a conflict. Returns the updated item. Item-to-item links use \u0060link\u0060.",
    "input": {
      "type": "object",
      "required": [
        "db",
        "id",
        "link"
      ],
      "properties": {
        "db": {
          "type": "string"
        },
        "id": {
          "type": "string"
        },
        "link": {
          "type": "object",
          "required": [
            "kind",
            "ref"
          ],
          "properties": {
            "kind": {
              "type": "string"
            },
            "ref": {
              "type": "string"
            },
            "label": {
              "type": "string"
            }
          }
        }
      }
    },
    "write": true
  },
  {
    "name": "link_remove",
    "description": "Remove the external pointer with this kind and ref from an item's \u0060links\u0060 array. not_found when there is no such link. Returns the updated item.",
    "input": {
      "type": "object",
      "required": [
        "db",
        "id",
        "kind",
        "ref"
      ],
      "properties": {
        "db": {
          "type": "string"
        },
        "id": {
          "type": "string"
        },
        "kind": {
          "type": "string"
        },
        "ref": {
          "type": "string"
        }
      }
    },
    "write": true
  },
  {
    "name": "reorder",
    "description": "Set \u0060order\u0060 = 10, 20, 30 ... for the listed ids in that sequence. Other items that had an order follow them in their current relative order; items with none stay unordered (after everything). Returns [{id, order, rev}] in the new order.",
    "input": {
      "type": "object",
      "required": [
        "db",
        "ids"
      ],
      "properties": {
        "db": {
          "type": "string"
        },
        "ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        }
      }
    },
    "write": true
  },
  {
    "name": "decide",
    "description": "Decide a needs-decision (or deferred) decision: option must exist; sets selected_option, status decided, decided_at, decided_by; adds the comment as kind 'decision' when given.",
    "input": {
      "type": "object",
      "required": [
        "id",
        "option"
      ],
      "properties": {
        "id": {
          "type": "string"
        },
        "option": {
          "type": "string"
        },
        "comment": {
          "type": "string"
        },
        "author": {
          "type": "string"
        }
      }
    },
    "write": true
  },
  {
    "name": "defer",
    "description": "Set a decision to deferred, with an optional note.",
    "input": {
      "type": "object",
      "required": [
        "id"
      ],
      "properties": {
        "id": {
          "type": "string"
        },
        "note": {
          "type": "string"
        },
        "author": {
          "type": "string"
        }
      }
    },
    "write": true
  },
  {
    "name": "reopen",
    "description": "Return a decision to needs-decision (clears selected_option, decided_at, decided_by, defer_note).",
    "input": {
      "type": "object",
      "required": [
        "id"
      ],
      "properties": {
        "id": {
          "type": "string"
        }
      }
    },
    "write": true
  },
  {
    "name": "inbox_add",
    "description": "Add an inbox item. \u0060path\u0060 is a filesystem path pointer only; the service never reads it.",
    "input": {
      "type": "object",
      "required": [
        "title"
      ],
      "properties": {
        "title": {
          "type": "string"
        },
        "kind": {
          "enum": [
            "topic",
            "idea",
            "question",
            "file",
            "link",
            "other"
          ]
        },
        "body": {
          "type": "string"
        },
        "path": {
          "type": "string"
        },
        "added_by": {
          "type": "string"
        }
      }
    },
    "write": true
  },
  {
    "name": "inbox_promote",
    "description": "Create an item in \u0060to_db\u0060 from an inbox item (title; body becomes notes; \u0060fields\u0060 override; path becomes a file link; related_ids point back), then mark the inbox item promoted with promoted_to. Returns {inbox, item}.",
    "input": {
      "type": "object",
      "required": [
        "id",
        "to_db"
      ],
      "properties": {
        "id": {
          "type": "string"
        },
        "to_db": {
          "type": "string"
        },
        "fields": {
          "type": "object"
        }
      }
    },
    "write": true
  },
  {
    "name": "inbox_dismiss",
    "description": "Mark an inbox item dismissed; an optional note is recorded as a comment.",
    "input": {
      "type": "object",
      "required": [
        "id"
      ],
      "properties": {
        "id": {
          "type": "string"
        },
        "note": {
          "type": "string"
        },
        "author": {
          "type": "string"
        }
      }
    },
    "write": true
  },
  {
    "name": "search",
    "description": "Case-insensitive search: every term must appear in the item's id, title or text fields (and comments). Returns [{db, id, title, status}].",
    "input": {
      "type": "object",
      "required": [
        "q"
      ],
      "properties": {
        "q": {
          "type": "string"
        },
        "db": {
          "type": "string"
        }
      }
    },
    "write": false
  },
  {
    "name": "schema",
    "description": "Describe one database's item schema: required fields, every known field (type, enum, bounds, description) and the enums by field. For forms and agents.",
    "input": {
      "type": "object",
      "required": [
        "db"
      ],
      "properties": {
        "db": {
          "type": "string"
        }
      }
    },
    "write": false
  },
  {
    "name": "torque_task",
    "description": "Read one Torque task and its comments (read-only). Returns {task{id,title,description,status,priority,project_id,epic_id,sprint_id,tags,created_at,updated_at}, comments[{id,author,content,created_at,updated_at}]}. Errors: not_found, unavailable (Torque down or slow).",
    "input": {
      "type": "object",
      "required": [
        "id"
      ],
      "properties": {
        "id": {
          "type": "string"
        }
      }
    },
    "write": false
  },
  {
    "name": "torque_tasks",
    "description": "List Torque tasks (read-only passthrough). Filters: project_id, epic_id, sprint_id, status, priority, tags, tags_any, tags_none, tag, q (text search), updated_after / updated_before (a date or RFC 3339 time), sort_by, sort_dir, limit (default 50, max 200), offset, cursor, include_total. Returns {items[{id,title,status,priority,project_id,epic_id,sprint_id,tags,updated_at}], meta{returned,limit,has_more,next_cursor,total?}}.",
    "input": {
      "type": "object",
      "required": [],
      "properties": {
        "project_id": {
          "type": "string"
        },
        "epic_id": {
          "type": "string"
        },
        "sprint_id": {
          "type": "string"
        },
        "status": {
          "type": "string"
        },
        "priority": {
          "type": "integer"
        },
        "tags": {
          "type": "string"
        },
        "tags_any": {
          "type": "string"
        },
        "tags_none": {
          "type": "string"
        },
        "tag": {
          "type": "string"
        },
        "q": {
          "type": "string"
        },
        "updated_after": {
          "type": "string"
        },
        "updated_before": {
          "type": "string"
        },
        "sort_by": {
          "type": "string"
        },
        "sort_dir": {
          "type": "string"
        },
        "limit": {
          "type": "integer"
        },
        "offset": {
          "type": "integer"
        },
        "cursor": {
          "type": "string"
        },
        "include_total": {
          "type": "boolean"
        }
      }
    },
    "write": false
  },
  {
    "name": "torque_titles",
    "description": "Batch-resolve Torque task ids to {id: {title,status,priority} | null} (read-only; unknown ids map to null; at most 100 ids). For chips and tables.",
    "input": {
      "type": "object",
      "required": [
        "ids"
      ],
      "properties": {
        "ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        }
      }
    },
    "write": false
  },
  {
    "name": "torque_projects",
    "description": "List Torque projects (read-only). Optional status. Returns [{id,name,status}] (all pages, up to 1000).",
    "input": {
      "type": "object",
      "required": [],
      "properties": {
        "status": {
          "type": "string"
        }
      }
    },
    "write": false
  },
  {
    "name": "torque_epics",
    "description": "List Torque epics (read-only). Optional project_id, status. Returns [{id,name,status,project_id}] (up to 1000).",
    "input": {
      "type": "object",
      "required": [],
      "properties": {
        "project_id": {
          "type": "string"
        },
        "status": {
          "type": "string"
        }
      }
    },
    "write": false
  },
  {
    "name": "torque_sprints",
    "description": "List Torque sprints (read-only). Optional project_id, epic_id, status. Returns [{id,name,status,epic_id,project_id,goal}] (goal cut to 300 chars; up to 1000).",
    "input": {
      "type": "object",
      "required": [],
      "properties": {
        "project_id": {
          "type": "string"
        },
        "epic_id": {
          "type": "string"
        },
        "status": {
          "type": "string"
        }
      }
    },
    "write": false
  },
  {
    "name": "torque_facets",
    "description": "Counts of Torque tasks by dimension for a filter (read-only passthrough of Torque's task facets). Filters as torque_tasks (project_id, epic_id, sprint_id, status (comma list ok), priority, tags, tags_any, tags_none, tag, q); dimensions[] from status, priority, tags, project_id, epic_id, sprint_id (default all). Returns {matching_count, dimensions, facets[{dimension, buckets[{value,count}], total_distinct, truncated}]}.",
    "input": {
      "type": "object",
      "required": [],
      "properties": {
        "project_id": {
          "type": "string"
        },
        "epic_id": {
          "type": "string"
        },
        "sprint_id": {
          "type": "string"
        },
        "status": {
          "type": "string"
        },
        "priority": {
          "type": "integer"
        },
        "tags": {
          "type": "string"
        },
        "tags_any": {
          "type": "string"
        },
        "tags_none": {
          "type": "string"
        },
        "tag": {
          "type": "string"
        },
        "q": {
          "type": "string"
        },
        "dimensions": {
          "type": "array",
          "items": {
            "type": "string"
          }
        }
      }
    },
    "write": false
  },
  {
    "name": "board",
    "description": "The departure board: what is in flight, meaning actively worked (Torque doing with an update or comment within active_hours, default 2; Torque review; roadmap in-progress), recently landed (Torque done, roadmap landed/done within 7 days, decisions decided, within recent_hours), pre-flight, meaning preparing but not executing (Torque queued, Torque doing with no recent activity, roadmap adopting, roadmap planned for now/next) and holds waiting on the owner (decisions needing a decision, new inbox items, open high risks). Read-only. Torque unavailable never fails it: the tracker rows still come back and \u0060notices\u0060 names the failure. Returns {generated_at, recent_hours, limit, sections{in_flight,landed,pre_flight,holds: [{source, id, title, status, priority?, workstream?, updated, db?}]}, counts, totals, notices[]}.",
    "input": {
      "type": "object",
      "required": [],
      "properties": {
        "recent_hours": {
          "type": "number"
        },
        "active_hours": {
          "type": "number"
        },
        "limit": {
          "type": "integer"
        }
      }
    },
    "write": false
  },
  {
    "name": "contract",
    "description": "Describe this registry: operation names, descriptions, inputs and whether they write. For the MCP server and docs.",
    "input": {
      "type": "object",
      "required": [],
      "properties": {}
    },
    "write": false
  },
  {
    "name": "migrate",
    "description": "Idempotent migration/seed: rev=1 where missing; decisions 'active' -> 'decided'; creates inbox.json with its seed items when absent; seeds DEC-017..019 when absent; turns ID-cliproxy-review into a pointer to DEC-017. Returns {changed:[db], seeded:[id]}.",
    "input": {
      "type": "object",
      "required": [],
      "properties": {}
    },
    "write": true
  }
]`

const itemSchemasJSON = `{
  "workstreams": {
    "$id": "portfolio/workstreams@1",
    "description": "Workstreams: the Tether workstream containers and the Torque projects they map to, with current state.",
    "$defs": {
      "link": {
        "type": "object",
        "required": [
          "kind",
          "ref"
        ],
        "additionalProperties": false,
        "properties": {
          "kind": {
            "enum": [
              "torque",
              "tesseract",
              "tether",
              "pr",
              "adr",
              "url",
              "file",
              "commit",
              "workstream"
            ]
          },
          "ref": {
            "type": "string"
          },
          "label": {
            "type": "string"
          }
        }
      },
      "comment": {
        "type": "object",
        "required": [
          "id",
          "author",
          "text",
          "created"
        ],
        "additionalProperties": true,
        "properties": {
          "id": {
            "type": "string",
            "description": "c-<n>, unique within the item"
          },
          "author": {
            "type": "string"
          },
          "text": {
            "type": "string"
          },
          "created": {
            "type": "string",
            "description": "ISO timestamp"
          },
          "kind": {
            "enum": [
              "comment",
              "decision"
            ]
          }
        }
      }
    },
    "item": {
      "type": "object",
      "required": [
        "id",
        "title",
        "status"
      ],
      "additionalProperties": true,
      "properties": {
        "id": {
          "type": "string",
          "description": "stable id, prefix + slug; never reused"
        },
        "title": {
          "type": "string"
        },
        "tldr": {
          "type": "string",
          "description": "optional: one short paragraph. The central concept and the tension: what pulls different ways, what is at stake or undecided"
        },
        "notes": {
          "type": "string"
        },
        "tags": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "links": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/link"
          }
        },
        "source": {
          "type": "string",
          "description": "where this came from, e.g. session date or message id"
        },
        "created": {
          "type": "string",
          "format": "date"
        },
        "updated": {
          "type": "string",
          "format": "date"
        },
        "status": {
          "enum": [
            "active",
            "paused",
            "planned",
            "parked",
            "done",
            "unreviewed"
          ]
        },
        "phase": {
          "enum": [
            "landing",
            "adoption",
            "planning",
            "maintenance",
            "unknown"
          ]
        },
        "tether_workstream_id": {
          "type": "string"
        },
        "torque_project_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "torque_tag": {
          "type": "string"
        },
        "owner": {
          "type": "string",
          "description": "team or orchestrator currently accountable, or 'unallocated'"
        },
        "goal": {
          "type": "string"
        },
        "state": {
          "type": "string",
          "description": "what is true now; dated in updated"
        },
        "next_step": {
          "type": "string"
        },
        "blockers": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "priority_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "roadmap_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "rev": {
          "type": "integer",
          "minimum": 0,
          "description": "write counter, starts at 1, incremented by the service on every write; missing counts as 0"
        },
        "order": {
          "type": "number",
          "description": "list order within the database; items without it sort after those with it"
        },
        "priority": {
          "type": "integer",
          "minimum": 1,
          "maximum": 5,
          "description": "1 highest, same meaning as Torque"
        },
        "author": {
          "type": "string",
          "description": "who created the item (set by the service on create)"
        },
        "comments": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/comment"
          }
        },
        "related_ids": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "description": "generic link to item ids in any database"
        }
      }
    }
  },
  "decisions": {
    "$id": "portfolio/decisions@1",
    "description": "Decision log index. The durable record is Tesseract or an ADR; this is the quick-reference list with pointers.",
    "$defs": {
      "link": {
        "type": "object",
        "required": [
          "kind",
          "ref"
        ],
        "additionalProperties": false,
        "properties": {
          "kind": {
            "enum": [
              "torque",
              "tesseract",
              "tether",
              "pr",
              "adr",
              "url",
              "file",
              "commit",
              "workstream"
            ]
          },
          "ref": {
            "type": "string"
          },
          "label": {
            "type": "string"
          }
        }
      },
      "comment": {
        "type": "object",
        "required": [
          "id",
          "author",
          "text",
          "created"
        ],
        "additionalProperties": true,
        "properties": {
          "id": {
            "type": "string",
            "description": "c-<n>, unique within the item"
          },
          "author": {
            "type": "string"
          },
          "text": {
            "type": "string"
          },
          "created": {
            "type": "string",
            "description": "ISO timestamp"
          },
          "kind": {
            "enum": [
              "comment",
              "decision"
            ]
          }
        }
      },
      "option": {
        "type": "object",
        "required": [
          "id",
          "label"
        ],
        "additionalProperties": true,
        "properties": {
          "id": {
            "type": "string",
            "description": "a, b, c ..."
          },
          "label": {
            "type": "string"
          },
          "summary": {
            "type": "string"
          },
          "pros": {
            "type": "string"
          },
          "cons": {
            "type": "string"
          },
          "recommended": {
            "type": "boolean"
          },
          "proposed_by": {
            "type": "string"
          }
        }
      }
    },
    "item": {
      "type": "object",
      "required": [
        "id",
        "title",
        "status"
      ],
      "additionalProperties": true,
      "properties": {
        "id": {
          "type": "string",
          "description": "stable id, prefix + slug; never reused"
        },
        "title": {
          "type": "string"
        },
        "tldr": {
          "type": "string",
          "description": "optional: one short paragraph. The central concept and the tension: what pulls different ways, what is at stake or undecided"
        },
        "notes": {
          "type": "string"
        },
        "tags": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "links": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/link"
          }
        },
        "source": {
          "type": "string",
          "description": "where this came from, e.g. session date or message id"
        },
        "created": {
          "type": "string",
          "format": "date"
        },
        "updated": {
          "type": "string",
          "format": "date"
        },
        "date": {
          "type": "string",
          "format": "date"
        },
        "decided_by": {
          "type": "string"
        },
        "status": {
          "enum": [
            "needs-decision",
            "decided",
            "deferred",
            "superseded",
            "reversed"
          ]
        },
        "area": {
          "type": "string"
        },
        "decision": {
          "type": "string"
        },
        "rationale": {
          "type": "string"
        },
        "alternatives": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "supersedes": {
          "type": "string"
        },
        "superseded_by": {
          "type": "string"
        },
        "rev": {
          "type": "integer",
          "minimum": 0,
          "description": "write counter, starts at 1, incremented by the service on every write; missing counts as 0"
        },
        "order": {
          "type": "number",
          "description": "list order within the database; items without it sort after those with it"
        },
        "priority": {
          "type": "integer",
          "minimum": 1,
          "maximum": 5,
          "description": "1 highest, same meaning as Torque"
        },
        "author": {
          "type": "string",
          "description": "who created the item (set by the service on create)"
        },
        "comments": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/comment"
          }
        },
        "related_ids": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "description": "generic link to item ids in any database"
        },
        "options": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/option"
          }
        },
        "selected_option": {
          "type": "string",
          "description": "id of the chosen option"
        },
        "decided_at": {
          "type": "string",
          "format": "date"
        },
        "defer_note": {
          "type": "string"
        }
      }
    }
  },
  "risks": {
    "$id": "portfolio/risks@1",
    "description": "Risks, gaps, debt and known limitations worth keeping in view at the portfolio level.",
    "$defs": {
      "link": {
        "type": "object",
        "required": [
          "kind",
          "ref"
        ],
        "additionalProperties": false,
        "properties": {
          "kind": {
            "enum": [
              "torque",
              "tesseract",
              "tether",
              "pr",
              "adr",
              "url",
              "file",
              "commit",
              "workstream"
            ]
          },
          "ref": {
            "type": "string"
          },
          "label": {
            "type": "string"
          }
        }
      },
      "comment": {
        "type": "object",
        "required": [
          "id",
          "author",
          "text",
          "created"
        ],
        "additionalProperties": true,
        "properties": {
          "id": {
            "type": "string",
            "description": "c-<n>, unique within the item"
          },
          "author": {
            "type": "string"
          },
          "text": {
            "type": "string"
          },
          "created": {
            "type": "string",
            "description": "ISO timestamp"
          },
          "kind": {
            "enum": [
              "comment",
              "decision"
            ]
          }
        }
      }
    },
    "item": {
      "type": "object",
      "required": [
        "id",
        "title",
        "kind",
        "severity",
        "status"
      ],
      "additionalProperties": true,
      "properties": {
        "id": {
          "type": "string",
          "description": "stable id, prefix + slug; never reused"
        },
        "title": {
          "type": "string"
        },
        "tldr": {
          "type": "string",
          "description": "optional: one short paragraph. The central concept and the tension: what pulls different ways, what is at stake or undecided"
        },
        "notes": {
          "type": "string"
        },
        "tags": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "links": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/link"
          }
        },
        "source": {
          "type": "string",
          "description": "where this came from, e.g. session date or message id"
        },
        "created": {
          "type": "string",
          "format": "date"
        },
        "updated": {
          "type": "string",
          "format": "date"
        },
        "kind": {
          "enum": [
            "risk",
            "gap",
            "debt",
            "limitation",
            "security",
            "process"
          ]
        },
        "severity": {
          "enum": [
            "low",
            "medium",
            "high"
          ]
        },
        "status": {
          "enum": [
            "open",
            "mitigating",
            "accepted",
            "closed"
          ]
        },
        "description": {
          "type": "string"
        },
        "impact": {
          "type": "string"
        },
        "mitigation": {
          "type": "string"
        },
        "owner": {
          "type": "string"
        },
        "raised": {
          "type": "string",
          "format": "date"
        },
        "roadmap_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "workstream_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "rev": {
          "type": "integer",
          "minimum": 0,
          "description": "write counter, starts at 1, incremented by the service on every write; missing counts as 0"
        },
        "order": {
          "type": "number",
          "description": "list order within the database; items without it sort after those with it"
        },
        "priority": {
          "type": "integer",
          "minimum": 1,
          "maximum": 5,
          "description": "1 highest, same meaning as Torque"
        },
        "author": {
          "type": "string",
          "description": "who created the item (set by the service on create)"
        },
        "comments": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/comment"
          }
        },
        "related_ids": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "description": "generic link to item ids in any database"
        }
      }
    }
  },
  "inbox": {
    "$id": "portfolio/inbox@1",
    "description": "Inbox: things to look at that are not yet items elsewhere. Triaged, then promoted into another database or dismissed.",
    "$defs": {
      "link": {
        "type": "object",
        "required": [
          "kind",
          "ref"
        ],
        "additionalProperties": false,
        "properties": {
          "kind": {
            "enum": [
              "torque",
              "tesseract",
              "tether",
              "pr",
              "adr",
              "url",
              "file",
              "commit",
              "workstream"
            ]
          },
          "ref": {
            "type": "string"
          },
          "label": {
            "type": "string"
          }
        }
      },
      "comment": {
        "type": "object",
        "required": [
          "id",
          "author",
          "text",
          "created"
        ],
        "additionalProperties": true,
        "properties": {
          "id": {
            "type": "string",
            "description": "c-<n>, unique within the item"
          },
          "author": {
            "type": "string"
          },
          "text": {
            "type": "string"
          },
          "created": {
            "type": "string",
            "description": "ISO timestamp"
          },
          "kind": {
            "enum": [
              "comment",
              "decision"
            ]
          }
        }
      }
    },
    "item": {
      "type": "object",
      "required": [
        "id",
        "title"
      ],
      "additionalProperties": true,
      "properties": {
        "id": {
          "type": "string",
          "description": "IN-001, sequential; never reused"
        },
        "title": {
          "type": "string"
        },
        "tldr": {
          "type": "string",
          "description": "optional: one short paragraph. The central concept and the tension: what pulls different ways, what is at stake or undecided"
        },
        "kind": {
          "enum": [
            "topic",
            "idea",
            "question",
            "file",
            "link",
            "other"
          ]
        },
        "status": {
          "enum": [
            "new",
            "triaged",
            "promoted",
            "dismissed"
          ]
        },
        "body": {
          "type": "string"
        },
        "path": {
          "type": "string",
          "description": "filesystem path pointer only; the service never reads it"
        },
        "added_by": {
          "type": "string"
        },
        "promoted_to": {
          "type": "object",
          "required": [
            "db",
            "id"
          ],
          "properties": {
            "db": {
              "type": "string"
            },
            "id": {
              "type": "string"
            }
          }
        },
        "notes": {
          "type": "string"
        },
        "tags": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "links": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/link"
          }
        },
        "source": {
          "type": "string"
        },
        "created": {
          "type": "string",
          "format": "date"
        },
        "updated": {
          "type": "string",
          "format": "date"
        },
        "rev": {
          "type": "integer",
          "minimum": 0,
          "description": "write counter, starts at 1, incremented by the service on every write; missing counts as 0"
        },
        "order": {
          "type": "number",
          "description": "list order within the database; items without it sort after those with it"
        },
        "priority": {
          "type": "integer",
          "minimum": 1,
          "maximum": 5,
          "description": "1 highest, same meaning as Torque"
        },
        "comments": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/comment"
          }
        },
        "related_ids": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "description": "generic link to item ids in any database"
        }
      }
    }
  },
  "ideas": {
    "$id": "portfolio/ideas@1",
    "description": "Explore / discuss / ideas: things to think about, decide or follow up, before they become roadmap items.",
    "$defs": {
      "link": {
        "type": "object",
        "required": [
          "kind",
          "ref"
        ],
        "additionalProperties": false,
        "properties": {
          "kind": {
            "enum": [
              "torque",
              "tesseract",
              "tether",
              "pr",
              "adr",
              "url",
              "file",
              "commit",
              "workstream"
            ]
          },
          "ref": {
            "type": "string"
          },
          "label": {
            "type": "string"
          }
        }
      },
      "comment": {
        "type": "object",
        "required": [
          "id",
          "author",
          "text",
          "created"
        ],
        "additionalProperties": true,
        "properties": {
          "id": {
            "type": "string",
            "description": "c-<n>, unique within the item"
          },
          "author": {
            "type": "string"
          },
          "text": {
            "type": "string"
          },
          "created": {
            "type": "string",
            "description": "ISO timestamp"
          },
          "kind": {
            "enum": [
              "comment",
              "decision"
            ]
          }
        }
      }
    },
    "item": {
      "type": "object",
      "required": [
        "id",
        "title",
        "kind",
        "status"
      ],
      "additionalProperties": true,
      "properties": {
        "id": {
          "type": "string",
          "description": "stable id, prefix + slug; never reused"
        },
        "title": {
          "type": "string"
        },
        "tldr": {
          "type": "string",
          "description": "optional: one short paragraph. The central concept and the tension: what pulls different ways, what is at stake or undecided"
        },
        "notes": {
          "type": "string"
        },
        "tags": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "links": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/link"
          }
        },
        "source": {
          "type": "string",
          "description": "where this came from, e.g. session date or message id"
        },
        "created": {
          "type": "string",
          "format": "date"
        },
        "updated": {
          "type": "string",
          "format": "date"
        },
        "kind": {
          "enum": [
            "idea",
            "question",
            "exploration",
            "discussion",
            "decision-needed"
          ]
        },
        "status": {
          "enum": [
            "new",
            "discussing",
            "decided",
            "parked",
            "dropped"
          ]
        },
        "summary": {
          "type": "string"
        },
        "context": {
          "type": "string"
        },
        "options": {
          "type": "array",
          "items": {
            "type": "object",
            "required": [
              "label"
            ],
            "properties": {
              "label": {
                "type": "string"
              },
              "pros": {
                "type": "string"
              },
              "cons": {
                "type": "string"
              }
            }
          }
        },
        "recommendation": {
          "type": "string"
        },
        "decision": {
          "type": "string"
        },
        "discuss_in": {
          "enum": [
            "planning",
            "next-session",
            "anytime",
            "blocked"
          ]
        },
        "priority_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "roadmap_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "related_ids": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "description": "generic link to item ids in any database"
        },
        "rev": {
          "type": "integer",
          "minimum": 0,
          "description": "write counter, starts at 1, incremented by the service on every write; missing counts as 0"
        },
        "order": {
          "type": "number",
          "description": "list order within the database; items without it sort after those with it"
        },
        "priority": {
          "type": "integer",
          "minimum": 1,
          "maximum": 5,
          "description": "1 highest, same meaning as Torque"
        },
        "author": {
          "type": "string",
          "description": "who created the item (set by the service on create)"
        },
        "comments": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/comment"
          }
        }
      }
    }
  },
  "priorities": {
    "$id": "portfolio/priorities@1",
    "description": "The owner's top-level priorities. Each points at the roadmap items, workstreams and ideas that serve it.",
    "$defs": {
      "link": {
        "type": "object",
        "required": [
          "kind",
          "ref"
        ],
        "additionalProperties": false,
        "properties": {
          "kind": {
            "enum": [
              "torque",
              "tesseract",
              "tether",
              "pr",
              "adr",
              "url",
              "file",
              "commit",
              "workstream"
            ]
          },
          "ref": {
            "type": "string"
          },
          "label": {
            "type": "string"
          }
        }
      },
      "comment": {
        "type": "object",
        "required": [
          "id",
          "author",
          "text",
          "created"
        ],
        "additionalProperties": true,
        "properties": {
          "id": {
            "type": "string",
            "description": "c-<n>, unique within the item"
          },
          "author": {
            "type": "string"
          },
          "text": {
            "type": "string"
          },
          "created": {
            "type": "string",
            "description": "ISO timestamp"
          },
          "kind": {
            "enum": [
              "comment",
              "decision"
            ]
          }
        }
      }
    },
    "item": {
      "type": "object",
      "required": [
        "id",
        "title",
        "rank",
        "status"
      ],
      "additionalProperties": true,
      "properties": {
        "id": {
          "type": "string",
          "description": "stable id, prefix + slug; never reused"
        },
        "title": {
          "type": "string"
        },
        "tldr": {
          "type": "string",
          "description": "optional: one short paragraph. The central concept and the tension: what pulls different ways, what is at stake or undecided"
        },
        "notes": {
          "type": "string"
        },
        "tags": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "links": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/link"
          }
        },
        "source": {
          "type": "string",
          "description": "where this came from, e.g. session date or message id"
        },
        "created": {
          "type": "string",
          "format": "date"
        },
        "updated": {
          "type": "string",
          "format": "date"
        },
        "rank": {
          "type": "integer",
          "description": "order as given by the owner; lower is first. Not a confirmed ranking unless notes say so"
        },
        "status": {
          "enum": [
            "active",
            "exploring",
            "queued",
            "done",
            "dropped"
          ]
        },
        "summary": {
          "type": "string"
        },
        "sub_items": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "open_questions": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "roadmap_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "workstream_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "idea_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "rev": {
          "type": "integer",
          "minimum": 0,
          "description": "write counter, starts at 1, incremented by the service on every write; missing counts as 0"
        },
        "order": {
          "type": "number",
          "description": "list order within the database; items without it sort after those with it"
        },
        "priority": {
          "type": "integer",
          "minimum": 1,
          "maximum": 5,
          "description": "1 highest, same meaning as Torque"
        },
        "author": {
          "type": "string",
          "description": "who created the item (set by the service on create)"
        },
        "comments": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/comment"
          }
        },
        "related_ids": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "description": "generic link to item ids in any database"
        }
      }
    }
  },
  "roadmap": {
    "$id": "portfolio/roadmap@1",
    "description": "Roadmap: higher-level capabilities and milestones, not tasks. Task and architecture detail is handed to other agents.",
    "$defs": {
      "link": {
        "type": "object",
        "required": [
          "kind",
          "ref"
        ],
        "additionalProperties": false,
        "properties": {
          "kind": {
            "enum": [
              "torque",
              "tesseract",
              "tether",
              "pr",
              "adr",
              "url",
              "file",
              "commit",
              "workstream"
            ]
          },
          "ref": {
            "type": "string"
          },
          "label": {
            "type": "string"
          }
        }
      },
      "comment": {
        "type": "object",
        "required": [
          "id",
          "author",
          "text",
          "created"
        ],
        "additionalProperties": true,
        "properties": {
          "id": {
            "type": "string",
            "description": "c-<n>, unique within the item"
          },
          "author": {
            "type": "string"
          },
          "text": {
            "type": "string"
          },
          "created": {
            "type": "string",
            "description": "ISO timestamp"
          },
          "kind": {
            "enum": [
              "comment",
              "decision"
            ]
          }
        }
      }
    },
    "item": {
      "type": "object",
      "required": [
        "id",
        "title",
        "area",
        "horizon",
        "status"
      ],
      "additionalProperties": true,
      "properties": {
        "id": {
          "type": "string",
          "description": "stable id, prefix + slug; never reused"
        },
        "title": {
          "type": "string"
        },
        "tldr": {
          "type": "string",
          "description": "optional: one short paragraph. The central concept and the tension: what pulls different ways, what is at stake or undecided"
        },
        "notes": {
          "type": "string"
        },
        "tags": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "links": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/link"
          }
        },
        "source": {
          "type": "string",
          "description": "where this came from, e.g. session date or message id"
        },
        "created": {
          "type": "string",
          "format": "date"
        },
        "updated": {
          "type": "string",
          "format": "date"
        },
        "area": {
          "enum": [
            "hub",
            "llm-gateway",
            "mesh",
            "gui",
            "plugin-platform",
            "fabric",
            "nanite",
            "harness",
            "policy",
            "tooling",
            "process"
          ]
        },
        "horizon": {
          "enum": [
            "now",
            "next",
            "later",
            "someday"
          ]
        },
        "status": {
          "enum": [
            "idea",
            "planned",
            "in-progress",
            "landed",
            "adopting",
            "done",
            "dropped"
          ]
        },
        "mvp": {
          "type": "boolean",
          "description": "true if it is part of the landing the other apps depend on"
        },
        "outcome": {
          "type": "string",
          "description": "what is true when this is done"
        },
        "depends_on": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "priority_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "workstream_ids": {
          "type": "array",
          "items": {
            "type": "string"
          }
        },
        "tasks": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "description": "Torque task ids; tasks stay in Torque, this is a pointer"
        },
        "rev": {
          "type": "integer",
          "minimum": 0,
          "description": "write counter, starts at 1, incremented by the service on every write; missing counts as 0"
        },
        "order": {
          "type": "number",
          "description": "list order within the database; items without it sort after those with it"
        },
        "priority": {
          "type": "integer",
          "minimum": 1,
          "maximum": 5,
          "description": "1 highest, same meaning as Torque"
        },
        "author": {
          "type": "string",
          "description": "who created the item (set by the service on create)"
        },
        "comments": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/comment"
          }
        },
        "related_ids": {
          "type": "array",
          "items": {
            "type": "string"
          },
          "description": "generic link to item ids in any database"
        }
      }
    }
  }
}`

const legacySeedsJSON = `{
  "decisions": [
    {
      "id": "DEC-017",
      "title": "Adopt CLIProxyAPI?",
      "status": "needs-decision",
      "decided_by": "",
      "options": [
        {
          "id": "a",
          "label": "Optional external backend, run as a separate service",
          "pros": "Reuses token refresh, protocol translation and streaming; contained as one HTTP integration",
          "cons": "Another credential store; compatibility churn stays in the project",
          "proposed_by": "portfolio-manager",
          "recommended": true
        },
        {
          "id": "b",
          "label": "Reimplement the capability inside Tether",
          "pros": "One system",
          "cons": "Substantial ongoing provider compatibility work",
          "proposed_by": "portfolio-manager"
        },
        {
          "id": "c",
          "label": "Skip; keep native harnesses only",
          "pros": "Nothing new to run",
          "cons": "No subscription-backed inference for our own harnesses",
          "proposed_by": "portfolio-manager"
        }
      ],
      "links": [
        {
          "kind": "file",
          "ref": "~/dev/inbox/portfolio-manager/cli-proxy-exploration.md",
          "label": "CLIProxy exploration"
        }
      ],
      "related_ids": [
        "ID-cliproxy-review",
        "PR-11"
      ],
      "source": "portfolio-manager session 2026-10-08"
    },
    {
      "id": "DEC-018",
      "title": "Two plugin hosts: what to do with mcp-host's in-process transport?",
      "status": "needs-decision",
      "decided_by": "",
      "options": [
        {
          "id": "a",
          "label": "Keep both for now",
          "pros": "No work; Station keeps working after the module-path change plus an Init factory",
          "cons": "Two lifecycle and grants models",
          "proposed_by": "portfolio-manager"
        },
        {
          "id": "b",
          "label": "Run in-process mode on plugin-host's driver",
          "pros": "One host, one lifecycle and grants model",
          "cons": "Dependency and scope review needed; real work",
          "proposed_by": "portfolio-manager"
        },
        {
          "id": "c",
          "label": "Withdraw in-process mode; use process-mode MCP servers only",
          "pros": "Removes the duplicate",
          "cons": "Withdraws an advertised feature; Station's clock-plugin style example goes away",
          "proposed_by": "portfolio-manager"
        }
      ],
      "links": [
        {
          "kind": "torque",
          "ref": "CW-20261007-0012"
        }
      ],
      "related_ids": [
        "ID-mcp-host-vs-plugin-host",
        "ID-station-mcp-via-plugins",
        "RK-005"
      ],
      "source": "portfolio-manager session 2026-10-08"
    },
    {
      "id": "DEC-019",
      "title": "Real auto permission mode: what happens when a call would have prompted?",
      "status": "needs-decision",
      "decided_by": "",
      "options": [
        {
          "id": "a",
          "label": "Deny it",
          "pros": "Simple; deny rules stay absolute",
          "cons": "Unattended agents stall on legitimate calls",
          "proposed_by": "portfolio-manager"
        },
        {
          "id": "b",
          "label": "Resolve it by policy",
          "pros": "No stall; auditable",
          "cons": "Needs the Policy Engine decision point",
          "proposed_by": "portfolio-manager"
        },
        {
          "id": "c",
          "label": "Escalate to a human approver and wait",
          "pros": "Safest",
          "cons": "Breaks unattended operation",
          "proposed_by": "portfolio-manager"
        }
      ],
      "links": [
        {
          "kind": "torque",
          "ref": "CW-20261008-0004"
        }
      ],
      "related_ids": [
        "ID-real-auto-mode-semantics",
        "RM-auto-permission-mode",
        "PR-10"
      ],
      "source": "portfolio-manager session 2026-10-08"
    }
  ]
}`
