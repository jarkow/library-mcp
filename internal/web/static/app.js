(function () {
  "use strict";

  function getCSRFToken() {
    const meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.getAttribute("content") || "" : "";
  }

  async function apiFetch(url, options) {
    const opts = Object.assign({ headers: {} }, options || {});
    if (opts.method === "POST") {
      opts.headers["X-CSRF-Token"] = getCSRFToken();
      if (opts.body && typeof opts.body === "string" && !opts.headers["Content-Type"]) {
        opts.headers["Content-Type"] = "application/json";
      }
    }
    const resp = await fetch(url, opts);
    const data = await resp.json().catch(function () {
      return { error: "Unexpected response from server" };
    });
    if (!resp.ok) {
      throw new Error(data.error || "Request failed with status " + resp.status);
    }
    return data;
  }

  function showToast(message) {
    const banner = document.getElementById("toast-banner");
    if (!banner) return;
    banner.textContent = message;
    banner.classList.remove("hidden");
    window.setTimeout(function () {
      banner.classList.add("hidden");
    }, 5000);
  }

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (typeof text === "string") node.textContent = text;
    return node;
  }

  async function refreshStatusAndBooks() {
    try {
      const st = await apiFetch("/api/status");
      const qdrantPill = document.getElementById("qdrant-pill");
      qdrantPill.textContent = st.qdrant_online ? "Qdrant: Online (Hybrid RRF)" : "Qdrant: Offline (Local Fallback)";
      qdrantPill.className = st.qdrant_online ? "pill pill-ok" : "pill pill-warn";

      const ollamaPill = document.getElementById("ollama-pill");
      ollamaPill.textContent = st.ollama_online
        ? "Ollama: " + st.config.chat_model
        : "Ollama: Offline";
      ollamaPill.className = st.ollama_online ? "pill pill-ok" : "pill pill-warn";

      const statsPill = document.getElementById("stats-pill");
      statsPill.textContent =
        st.books_count + " Books | " + st.parents_count + " Parents | " + st.chunks_count + " Chunks";

      populateSettingsForm(st.config, st.resolved_books_dir);
    } catch (err) {
      showToast("Status check failed: " + err.message);
    }

    try {
      const data = await apiFetch("/api/books");
      renderBooksList(data.books || []);
    } catch (err) {
      showToast("Failed to load books: " + err.message);
    }
  }

  function populateSettingsForm(cfg, resolvedDir) {
    if (!cfg) return;
    document.getElementById("cfg-books-dir").value = cfg.books_dir || "";
    document.getElementById("cfg-resolved-dir").textContent = "Resolved path: " + (resolvedDir || "");
    document.getElementById("cfg-qdrant-url").value = cfg.qdrant_url || "";
    document.getElementById("cfg-ollama-url").value = cfg.ollama_url || "";
    document.getElementById("cfg-embed-model").value = cfg.embedding_model || "";
    document.getElementById("cfg-chat-model").value = cfg.chat_model || "";
    document.getElementById("cfg-parent-size").value = String(cfg.parent_chunk_size || 3200);
    document.getElementById("cfg-child-size").value = String(cfg.child_chunk_size || 650);
    document.getElementById("cfg-top-k").value = String(cfg.top_k || 6);
  }

  function renderBooksList(books) {
    const container = document.getElementById("books-list");
    const select = document.getElementById("book-filter-select");
    container.replaceChildren();

    const currentVal = select.value;
    select.replaceChildren();
    const defaultOpt = el("option", "", "All Indexed Books");
    defaultOpt.value = "";
    select.appendChild(defaultOpt);

    if (books.length === 0) {
      const empty = el("p", "muted", "No EPUBs indexed yet. Click 'Scan Books Dir' or upload an .epub file.");
      container.appendChild(empty);
      return;
    }

    books.forEach(function (b) {
      const opt = el("option", "", b.title + " (" + b.author + ")");
      opt.value = b.id;
      select.appendChild(opt);

      const card = el("div", "book-card");
      const title = el("div", "book-card-title", b.title);
      title.addEventListener("click", function () {
        openBookChapters(b.id);
      });

      const meta = el(
        "div",
        "book-card-meta",
        b.author +
          (b.published_year ? " (" + b.published_year + ")" : "") +
          (b.category ? " • [" + b.category + "]" : "") +
          " • " +
          b.chapter_count +
          " ch • " +
          b.parent_count +
          " sections"
      );

      let tagsRow = null;
      if (b.tags && b.tags.length > 0) {
        tagsRow = el("div", "book-card-meta", "Tags: #" + b.tags.slice(0, 5).join(" #"));
      }

      const actions = el("div", "book-card-actions");
      const readBtn = el("button", "btn btn-small btn-secondary", "Browse Chapters");
      readBtn.type = "button";
      readBtn.addEventListener("click", function () {
        openBookChapters(b.id);
      });

      const delBtn = el("button", "btn btn-small btn-danger", "Remove");
      delBtn.type = "button";
      delBtn.addEventListener("click", function () {
        deleteBook(b.id, b.title);
      });

      actions.appendChild(readBtn);
      actions.appendChild(delBtn);

      card.appendChild(title);
      card.appendChild(meta);
      if (tagsRow) card.appendChild(tagsRow);
      card.appendChild(actions);
      container.appendChild(card);
    });

    select.value = currentVal;
  }

  async function openBookChapters(bookId) {
    try {
      const data = await apiFetch("/api/book?id=" + encodeURIComponent(bookId));
      const book = data.book;
      const chapters = data.chapters || [];

      document.getElementById("reader-title").textContent = book.title;
      document.getElementById("reader-subtitle").textContent =
        book.author + " (" + chapters.length + " chapters)";

      const bar = document.getElementById("reader-chapters-bar");
      bar.replaceChildren();
      bar.classList.remove("hidden");

      chapters.forEach(function (ch) {
        const btn = el("button", "btn btn-small btn-secondary", ch.chapter_index + ". " + ch.title);
        btn.type = "button";
        btn.addEventListener("click", function () {
          loadChapter(ch.id, book.title);
        });
        bar.appendChild(btn);
      });

      if (chapters.length > 0) {
        await loadChapter(chapters[0].id, book.title);
      }
    } catch (err) {
      showToast("Could not load book details: " + err.message);
    }
  }

  async function loadChapter(chapterId, bookTitle) {
    try {
      const data = await apiFetch("/api/chapter?id=" + encodeURIComponent(chapterId));
      const ch = data.chapter;
      document.getElementById("reader-title").textContent = (bookTitle ? bookTitle + " — " : "") + ch.title;
      document.getElementById("reader-subtitle").textContent = "Full Chapter View (ID: " + ch.id + ")";
      document.getElementById("reader-content").textContent = ch.content || "";
    } catch (err) {
      showToast("Failed to load chapter: " + err.message);
    }
  }

  async function loadParentSection(parentId, bookTitle, chapterId) {
    try {
      const data = await apiFetch("/api/parent?id=" + encodeURIComponent(parentId));
      const p = data.parent;
      document.getElementById("reader-title").textContent = p.book_title + " — " + p.section_title;
      document.getElementById("reader-subtitle").textContent =
        "Promoted Parent Section (" + p.chapter_title + ")";

      const bar = document.getElementById("reader-chapters-bar");
      bar.replaceChildren();
      bar.classList.remove("hidden");

      if (chapterId) {
        const fullChapBtn = el("button", "btn btn-small btn-primary", "Open Full Surrounding Chapter");
        fullChapBtn.type = "button";
        fullChapBtn.addEventListener("click", function () {
          loadChapter(chapterId, bookTitle);
        });
        bar.appendChild(fullChapBtn);
      }

      document.getElementById("reader-content").textContent = p.content || "";
    } catch (err) {
      showToast("Failed to load parent section: " + err.message);
    }
  }

  async function deleteBook(bookId, title) {
    try {
      await apiFetch("/api/books/delete", {
        method: "POST",
        body: JSON.stringify({ book_id: bookId }),
      });
      showToast("Removed book: " + title);
      await refreshStatusAndBooks();
    } catch (err) {
      showToast("Delete failed: " + err.message);
    }
  }

  function createSourceCard(src, idx) {
    const ck = src.chunk;
    const card = el("div", "source-card");

    const topRow = el("div", "source-title-row");
    const badge = el(
      "strong",
      "",
      "[Source " + (idx + 1) + "] " + ck.book_title + " — " + ck.chapter_title + " > " + ck.section_title
    );

    const btnGroup = el("div", "book-card-actions");
    if (src.parent_id) {
      const parentBtn = el(
        "button",
        "btn btn-small btn-secondary",
        "View Parent Section (" + (src.child_hits || 1) + " child hits)"
      );
      parentBtn.type = "button";
      parentBtn.addEventListener("click", function () {
        loadParentSection(src.parent_id, ck.book_title, ck.chapter_id);
      });
      btnGroup.appendChild(parentBtn);
    }

    const chapBtn = el("button", "btn btn-small btn-secondary", "Read Full Chapter");
    chapBtn.type = "button";
    chapBtn.addEventListener("click", function () {
      loadChapter(ck.chapter_id, ck.book_title);
    });
    btnGroup.appendChild(chapBtn);

    topRow.appendChild(badge);
    topRow.appendChild(btnGroup);

    const meta = el(
      "div",
      "muted",
      "Author: " +
        ck.author +
        " | Score: " +
        Number(src.score || 0).toFixed(4) +
        " | Promoted Parent Chars: " +
        (src.parent_text ? src.parent_text.length : ck.text.length)
    );

    const snippet = el("pre", "source-snippet", src.parent_text || ck.text);

    card.appendChild(topRow);
    card.appendChild(meta);
    card.appendChild(snippet);
    return card;
  }

  async function refreshNotes() {
    const out = document.getElementById("notes-output");
    if (!out) return;
    try {
      const bookFilter = document.getElementById("book-filter-select").value;
      const res = await apiFetch("/api/notes?book_filter=" + encodeURIComponent(bookFilter));
      const notes = res.notes || [];
      out.replaceChildren();
      if (notes.length === 0) {
        out.appendChild(el("div", "empty-state", "No saved notes yet. Add a note above or click 'Compile Study Dossier'."));
        return;
      }
      notes.forEach(function (n) {
        const card = el("div", "source-card");
        const top = el("div", "source-title-row");
        top.appendChild(el("strong", "", n.title + (n.book_title ? " — " + n.book_title : "")));
        const del = el("button", "btn btn-small btn-danger", "Delete");
        del.type = "button";
        del.addEventListener("click", async function () {
          await apiFetch("/api/notes/delete", {
            method: "POST",
            body: JSON.stringify({ note_id: n.id }),
          });
          await refreshNotes();
        });
        top.appendChild(del);
        card.appendChild(top);
        card.appendChild(el("pre", "source-snippet", n.content));
        out.appendChild(card);
      });
    } catch (err) {
      out.replaceChildren(el("div", "empty-state", "Could not load notes: " + err.message));
    }
  }

  function initEventListeners() {
    const tabAsk = document.getElementById("tab-ask");
    const tabSearch = document.getElementById("tab-search");
    const tabNotes = document.getElementById("tab-notes");
    const viewAsk = document.getElementById("view-ask");
    const viewSearch = document.getElementById("view-search");
    const viewNotes = document.getElementById("view-notes");

    function activateTab(activeTab, activeView) {
      [tabAsk, tabSearch, tabNotes].forEach(function (t) {
        if (t) t.classList.toggle("active", t === activeTab);
      });
      [viewAsk, viewSearch, viewNotes].forEach(function (v) {
        if (v) v.classList.toggle("hidden", v !== activeView);
      });
    }

    tabAsk.addEventListener("click", function () {
      activateTab(tabAsk, viewAsk);
    });

    tabSearch.addEventListener("click", function () {
      activateTab(tabSearch, viewSearch);
    });

    if (tabNotes) {
      tabNotes.addEventListener("click", function () {
        activateTab(tabNotes, viewNotes);
        refreshNotes();
      });
    }

    document.getElementById("ask-form").addEventListener("submit", async function (e) {
      e.preventDefault();
      const input = document.getElementById("ask-input");
      const question = input.value.trim();
      if (!question) return;

      const bookFilter = document.getElementById("book-filter-select").value;
      const out = document.getElementById("ask-output");
      out.replaceChildren(el("div", "empty-state", "Running Qdrant Hybrid RRF + Parent-Child promotion + Ollama generation..."));

      try {
        const res = await apiFetch("/api/ask", {
          method: "POST",
          body: JSON.stringify({ question: question, book_filter: bookFilter }),
        });

        out.replaceChildren();
        const ansCard = el("div", "answer-card");
        const header = el(
          "div",
          "answer-header",
          "Model: " + res.model + " • Latency: " + res.duration_ms + " ms"
        );
        const body = el("div", "answer-body", res.answer);
        ansCard.appendChild(header);
        ansCard.appendChild(body);
        out.appendChild(ansCard);

        const sources = res.sources || [];
        if (sources.length > 0) {
          const srcHeading = el("h3", "muted", "Promoted Parent Sections (" + sources.length + ")");
          out.appendChild(srcHeading);
          sources.forEach(function (src, i) {
            out.appendChild(createSourceCard(src, i));
          });
        }
      } catch (err) {
        out.replaceChildren(el("div", "empty-state", "Error: " + err.message));
      }
    });

    document.getElementById("search-form").addEventListener("submit", async function (e) {
      e.preventDefault();
      const query = document.getElementById("search-input").value.trim();
      if (!query) return;

      const mode = document.getElementById("search-mode").value;
      const bookFilter = document.getElementById("book-filter-select").value;
      const out = document.getElementById("search-output");
      out.replaceChildren(el("div", "empty-state", "Searching Qdrant & promoting Parent Sections..."));

      try {
        const res = await apiFetch("/api/search", {
          method: "POST",
          body: JSON.stringify({ query: query, mode: mode, book_filter: bookFilter }),
        });
        out.replaceChildren();
        const results = res.results || [];
        if (results.length === 0) {
          out.appendChild(el("div", "empty-state", "No matching sections found."));
          return;
        }
        results.forEach(function (src, i) {
          out.appendChild(createSourceCard(src, i));
        });
      } catch (err) {
        out.replaceChildren(el("div", "empty-state", "Search failed: " + err.message));
      }
    });

    document.getElementById("btn-ingest").addEventListener("click", async function () {
      const btn = document.getElementById("btn-ingest");
      btn.disabled = true;
      btn.textContent = "Scanning...";
      try {
        const rep = await apiFetch("/api/ingest", {
          method: "POST",
          body: JSON.stringify({ force: false }),
        });
        showToast(
          "Indexed " +
            rep.indexed_books +
            " new books (" +
            rep.total_parents +
            " parent sections, " +
            rep.total_chunks +
            " child chunks, skipped " +
            rep.skipped_books +
            " unchanged)."
        );
        await refreshStatusAndBooks();
      } catch (err) {
        showToast("Ingest error: " + err.message);
      } finally {
        btn.disabled = false;
        btn.textContent = "Scan Books Dir";
      }
    });

    document.getElementById("epub-upload-input").addEventListener("change", async function (e) {
      const file = e.target.files && e.target.files[0];
      if (!file) return;
      const formData = new FormData();
      formData.append("epub", file);
      showToast("Uploading & indexing " + file.name + "...");
      try {
        const rep = await apiFetch("/api/upload", {
          method: "POST",
          body: formData,
        });
        showToast(
          "Uploaded " +
            rep.original_name +
            " (" +
            rep.parents_count +
            " parent sections, " +
            rep.chunks_count +
            " child chunks)."
        );
        await refreshStatusAndBooks();
      } catch (err) {
        showToast("Upload failed: " + err.message);
      } finally {
        e.target.value = "";
      }
    });

    const enrichBtn = document.getElementById("btn-enrich");
    if (enrichBtn) {
      enrichBtn.addEventListener("click", async function () {
        enrichBtn.disabled = true;
        enrichBtn.textContent = "Enriching...";
        try {
          const res = await apiFetch("/api/enrich", {
            method: "POST",
            body: JSON.stringify({ force: true }),
          });
          showToast("Enriched metadata, categories & tags for " + (res.count || 0) + " books.");
          await refreshStatusAndBooks();
        } catch (err) {
          showToast("Metadata enrich failed: " + err.message);
        } finally {
          enrichBtn.disabled = false;
          enrichBtn.textContent = "Enrich Metadata";
        }
      });
    }

    const dedupBtn = document.getElementById("btn-dedup");
    if (dedupBtn) {
      dedupBtn.addEventListener("click", async function () {
        dedupBtn.disabled = true;
        try {
          const rep = await apiFetch("/api/books/dedup", {
            method: "POST",
            body: JSON.stringify({ apply: true }),
          });
          showToast(
            "Deduplication complete: removed " +
              ((rep.removed_books && rep.removed_books.length) || 0) +
              " duplicate books (" +
              (rep.freed_chunks || 0) +
              " chunks freed)."
          );
          await refreshStatusAndBooks();
        } catch (err) {
          showToast("Deduplication failed: " + err.message);
        } finally {
          dedupBtn.disabled = false;
        }
      });
    }

    const noteForm = document.getElementById("note-form");
    if (noteForm) {
      noteForm.addEventListener("submit", async function (e) {
        e.preventDefault();
        const title = document.getElementById("note-title-input").value.trim();
        const content = document.getElementById("note-content-input").value.trim() || title;
        const bookId = document.getElementById("book-filter-select").value;
        try {
          await apiFetch("/api/notes", {
            method: "POST",
            body: JSON.stringify({ title: title, content: content, book_id: bookId }),
          });
          document.getElementById("note-title-input").value = "";
          document.getElementById("note-content-input").value = "";
          showToast("Saved note: " + title);
          await refreshNotes();
        } catch (err) {
          showToast("Save note failed: " + err.message);
        }
      });
    }

    const dossierBtn = document.getElementById("dossier-btn");
    if (dossierBtn) {
      dossierBtn.addEventListener("click", async function () {
        const topic = document.getElementById("note-title-input").value.trim();
        if (!topic) {
          showToast("Enter a topic in the title box first.");
          return;
        }
        const out = document.getElementById("notes-output");
        out.replaceChildren(el("div", "empty-state", "Compiling multi-book Study Dossier for '" + topic + "'..."));
        try {
          const res = await apiFetch("/api/dossier", {
            method: "POST",
            body: JSON.stringify({ topic: topic }),
          });
          document.getElementById("reader-title").textContent = "Study Dossier: " + topic;
          document.getElementById("reader-subtitle").textContent = "Multi-book synthesis, code examples & chapters";
          document.getElementById("reader-content").textContent = res.markdown || "";
          await refreshNotes();
        } catch (err) {
          showToast("Dossier error: " + err.message);
        }
      });
    }

    const modal = document.getElementById("settings-modal");
    document.getElementById("btn-settings").addEventListener("click", function () {
      modal.classList.remove("hidden");
    });
    document.getElementById("btn-close-settings").addEventListener("click", function () {
      modal.classList.add("hidden");
    });

    document.getElementById("settings-form").addEventListener("submit", async function (e) {
      e.preventDefault();
      const payload = {
        books_dir: document.getElementById("cfg-books-dir").value.trim(),
        qdrant_url: document.getElementById("cfg-qdrant-url").value.trim(),
        ollama_url: document.getElementById("cfg-ollama-url").value.trim(),
        embedding_model: document.getElementById("cfg-embed-model").value.trim(),
        chat_model: document.getElementById("cfg-chat-model").value.trim(),
        parent_chunk_size: parseInt(document.getElementById("cfg-parent-size").value, 10) || 3200,
        child_chunk_size: parseInt(document.getElementById("cfg-child-size").value, 10) || 650,
        top_k: parseInt(document.getElementById("cfg-top-k").value, 10) || 6,
      };
      try {
        await apiFetch("/api/config", {
          method: "POST",
          body: JSON.stringify(payload),
        });
        modal.classList.add("hidden");
        showToast("Configuration saved.");
        await refreshStatusAndBooks();
      } catch (err) {
        showToast("Failed to save settings: " + err.message);
      }
    });

    document.getElementById("btn-sync-qdrant").addEventListener("click", async function () {
      try {
        await apiFetch("/api/qdrant/sync", { method: "POST", body: "{}" });
        showToast("Synchronized local catalog chunks to Qdrant.");
        await refreshStatusAndBooks();
      } catch (err) {
        showToast("Qdrant sync error: " + err.message);
      }
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    initEventListeners();
    refreshStatusAndBooks();
  });
})();

