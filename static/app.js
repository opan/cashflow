// Copy-to-clipboard for any [data-copy="#selector"] button.
document.addEventListener('click', function (e) {
  var btn = e.target.closest('[data-copy]');
  if (!btn) return;
  var el = document.querySelector(btn.getAttribute('data-copy'));
  if (!el) return;

  function done() {
    var old = btn.textContent;
    btn.textContent = 'Tersalin!';
    setTimeout(function () { btn.textContent = old; }, 1500);
  }

  el.focus();
  el.select();
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(el.value).then(done, function () {
      document.execCommand('copy');
      done();
    });
  } else {
    document.execCommand('copy');
    done();
  }
});

// Unified entry form: swap labels/placeholders/required based on Jenis.
(function () {
  var sel = document.getElementById('typeSel');
  if (!sel) return;
  var form = document.getElementById('entryForm');
  var party = document.getElementById('partyInput');
  var desc = document.getElementById('descInput');
  var submit = document.getElementById('entrySubmit');

  function apply() {
    var t = sel.value; // "income" | "expense"
    form.querySelectorAll('.lbltext').forEach(function (sp) {
      var v = sp.getAttribute('data-' + t);
      if (v) sp.textContent = v;
    });
    if (t === 'expense') {
      party.required = false;
      desc.required = true;
      party.placeholder = 'cth: Toko / bendahara';
      desc.placeholder = 'cth: Beli kaos';
      submit.className = 'btn expense';
      submit.textContent = 'Simpan pengeluaran';
    } else {
      party.required = true;
      desc.required = false;
      party.placeholder = 'cth: Budi';
      desc.placeholder = 'cth: iuran September';
      submit.className = 'btn income';
      submit.textContent = 'Simpan pemasukan';
    }
  }
  sel.addEventListener('change', apply);
  apply();
})();

// Print button on the report page.
document.addEventListener('click', function (e) {
  if (e.target.closest('[data-print]')) window.print();
});

// Click/focus a read-only field (e.g. the share link) to select all of it.
document.addEventListener('focusin', function (e) {
  if (e.target.matches && e.target.matches('input[readonly]')) e.target.select();
});

// Live thousand-separator formatting for rupiah amount inputs, so long numbers
// are easy to read and hard to miscount ("50000" shows as "50.000").
(function () {
  function group(digits) {
    return digits.replace(/\B(?=(\d{3})+(?!\d))/g, '.');
  }
  function format(el) {
    var caret = el.selectionStart;
    var digitsBefore = el.value.slice(0, caret).replace(/\D/g, '').length;

    var digits = el.value.replace(/\D/g, '').replace(/^0+(?=\d)/, '');
    var formatted = group(digits);
    if (formatted === el.value) return; // nothing changed; leave caret alone
    el.value = formatted;

    // Restore the caret after the same number of digits it was before.
    var idx = 0, count = 0;
    while (idx < formatted.length && count < digitsBefore) {
      var c = formatted.charCodeAt(idx);
      if (c >= 48 && c <= 57) count++;
      idx++;
    }
    try { el.setSelectionRange(idx, idx); } catch (e) { /* ignore */ }
  }
  document.querySelectorAll('input.amount-input').forEach(function (el) {
    el.addEventListener('input', function () { format(el); });
    if (el.value) format(el);
  });
})();

// Dashboard: auto-suggest a slug from the plan name until the user edits it.
(function () {
  var title = document.getElementById('planTitle');
  var slug = document.getElementById('planSlug');
  if (!title || !slug) return;

  function slugify(s) {
    return s.toLowerCase().trim()
      .replace(/[^a-z0-9\s_-]/g, '')
      .replace(/[\s_-]+/g, '-')
      .replace(/^-+|-+$/g, '');
  }
  var edited = slug.value.length > 0;
  slug.addEventListener('input', function () { edited = true; });
  title.addEventListener('input', function () {
    if (!edited) slug.value = slugify(title.value);
  });
})();
