// Optional enhancement: all actions and manual refresh work without JavaScript.
const progress = document.querySelector('[data-status]');

if (progress) {
  const refresh = async () => {
    try {
      const response = await fetch(progress.dataset.status);

      if (!response.ok) {
        throw new Error('Status unavailable');
      }

      const job = await response.json();
      const jobName = job.Kind === 'lyrics' ? 'Lyrics lookup' : job.Kind;
      const remaining = Math.max(0, job.Total - job.Processed);

      const title = document.createElement('p');
      title.textContent = `${jobName} ${job.State}`;

      const message = document.createElement('p');
      message.textContent = job.Message;

      const counts = document.createElement('p');
      counts.textContent = `${job.Processed} processed / ${job.Total} total · ${remaining} remaining · ${job.Cached} cached · ${job.Failed} failed`;

      progress.replaceChildren(title, message, counts);

      document.querySelectorAll('[data-count]').forEach((element) => {
        const value = job.Counts[element.dataset.count];

        if (Number.isInteger(value)) {
          element.textContent = value;
        }
      });

      const createPlaylists = document.querySelector('[data-create-playlists]');

      if (createPlaylists) {
        createPlaylists.disabled = job.Counts.Total === 0 || job.Counts.MissingEvidence > 0;

        const help = document.querySelector('[data-create-playlists-help]');

        if (help) {
          if (job.Counts.Total === 0) {
            help.title = 'Add songs before creating playlists.';
          } else if (createPlaylists.disabled) {
            help.title = 'Every song needs lyrics or a description before playlists can be created.';
          } else {
            help.title = '';
          }
        }
      }

      document.querySelectorAll('[data-category-count]').forEach((element) => {
        element.textContent = job.Counts.Categories[element.dataset.categoryCount] || 0;
      });

      if (job.State === 'running' || job.State === 'waiting') {
        setTimeout(refresh, 2000);
      } else {
        location.reload();
      }
    } catch {
      progress.removeAttribute('data-status');
      progress.textContent = 'Progress updates unavailable. Refresh the page to check saved work.';
    }
  };

  setTimeout(refresh, 2000);
}

// Submit row assignments on selection; the normal button remains available without JS.
const scrollKey = `assignment-scroll:${location.pathname}`;

try {
  const savedScroll = sessionStorage.getItem(scrollKey);

  if (savedScroll !== null) {
    sessionStorage.removeItem(scrollKey);
    requestAnimationFrame(() => window.scrollTo(0, Number(savedScroll)));
  }
} catch {
  // Restoring scroll is optional when browser storage is unavailable.
}

document.querySelectorAll('.assignment-form').forEach((form) => {
  const select = form.querySelector('select');
  const button = form.querySelector('button');
  const error = form.querySelector('[data-assignment-error]');
  let savedValue = select.value;

  select.addEventListener('change', () => form.requestSubmit());

  form.addEventListener('submit', async (event) => {
    event.preventDefault();

    const data = new URLSearchParams(new FormData(form));
    select.disabled = true;
    error.hidden = true;

    try {
      const response = await fetch(form.action, { method: 'POST', body: data });

      if (!response.ok) {
        throw new Error('Could not save the assignment.');
      }

      const message = new URL(response.url).searchParams.get('message');

      if (message) {
        throw new Error(message);
      }

      savedValue = select.value;

      try {
        sessionStorage.setItem(scrollKey, String(window.scrollY));
      } catch {
        // Saving scroll position is optional when browser storage is unavailable.
      }

      location.reload();
    } catch (failure) {
      select.value = savedValue;
      select.disabled = false;
      error.textContent = failure.message || 'Could not save the assignment.';
      error.hidden = false;
      button.hidden = false;
    }
  });

  button.hidden = true;
});

// Show immediate feedback while standard form submissions are processing.
document.querySelectorAll('form[method="post"]:not(.assignment-form)').forEach((form) => {
  form.addEventListener('submit', (event) => {
    const button = event.submitter;

    if (!button || button.querySelector('.loading-spinner')) {
      return;
    }

    const spinner = document.createElement('span');
    spinner.className = 'loading-spinner';
    spinner.setAttribute('aria-hidden', 'true');

    button.append(spinner);
    button.setAttribute('aria-busy', 'true');
  });
});
