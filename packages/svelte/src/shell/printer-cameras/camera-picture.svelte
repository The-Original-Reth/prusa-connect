<!--
  One camera's picture: its still frame and its live view over it, which
  starts by itself once the camera can stream. Play sits in the middle of the picture while the camera
  can stream, and stop in a corner while it does. The Cameras tab, the
  printer's Now tab, the dashboard's camera tile and the printer list's camera
  view all draw a camera this way.
-->
<script lang="ts">
  import PlayIcon from '@lucide/svelte/icons/play';
  import SquareIcon from '@lucide/svelte/icons/square';
  import type { Snippet } from 'svelte';

  import type { Camera } from '../../data';
  import { t } from '../../i18n';
  import { AspectRatio, Button } from '../../primitives';
  import type { CameraPictures, CameraWatch } from './types';
  import WebrtcView from './webrtc-view.svelte';

  let {
    camera,
    pictures,
    alt,
    watch,
    view,
    playing = $bindable(false),
    class: className,
    children,
  }: {
    camera: Camera;
    pictures: CameraPictures;
    /** What a screen reader says the picture is. */
    alt: string;
    watch?: CameraWatch | undefined;
    /** A module's own live view of the camera, drawn in place of the core's. */
    view?: Snippet<[Camera]> | undefined;
    /** Whether the live view runs. A stream runs only while somebody asked for it. */
    playing?: boolean;
    class?: string | undefined;
    /** Drawn over the picture, such as a badge. */
    children?: Snippet;
  } = $props();

  // The last still that loaded. Each one is fetched off screen first, so a
  // camera with no frame yet never shows a broken picture and its alt text.
  let still = $state<string | undefined>(undefined);
  // Derived, so a new pictures object with the same address probes nothing.
  const wanted = $derived(pictures.still);
  $effect(() => {
    const next = wanted;
    if (next === undefined || !camera.serving) {
      still = undefined;

      return;
    }
    // The still is hidden under a playing live view, so it waits for stop.
    if (live) return;
    const probe = new Image();
    probe.onload = () => {
      still = next;
    };
    // A camera with no picture any more answers with none.
    probe.onerror = () => {
      still = undefined;
    };
    probe.src = next;

    return () => {
      probe.onload = null;
      probe.onerror = null;
    };
  });

  // A module's own view says nothing of whether the camera has anything to
  // show, so it is offered once the camera sent a picture.
  const livable = $derived(
    (view !== undefined && still !== undefined) ||
      pictures.live !== undefined ||
      (camera.watch && watch !== undefined),
  );
  const live = $derived(livable && playing);
  // Whether the last live view did not start, which the picture says until
  // it is played again.
  let stalled = $state(false);
  const fail = (): void => {
    playing = false;
    stalled = true;
  };

  const toggle = (): void => {
    stalled = false;
    playing = !playing;
  };

  // The live view starts by itself, once, as soon as the camera can stream:
  // nobody has to press play on every page. Stop keeps it stopped here, and
  // a view that fails is not started again until somebody presses play.
  let autoplayed = false;
  $effect(() => {
    if (autoplayed || !livable || !camera.serving) return;
    autoplayed = true;
    if (!playing) playing = true;
  });
</script>

<AspectRatio
  ratio={16 / 9}
  class={['relative overflow-hidden bg-muted', className]}
>
  <!-- The words sit under the pictures, so they show until a picture loads
       over them. A playing live view sits over the still frame: a camera
       that pushes a picture every half minute has an empty stream until its
       next one, and the last still shows through. -->
  {#if still === undefined || live || !camera.serving}
    <p
      class="absolute inset-0 flex items-center justify-center p-4 text-center text-sm text-muted-foreground"
    >
      {#if !camera.serving}
        {t('printerDetailCameras.notRunningNote')}
      {:else if pictures.still === undefined && !livable}
        {t('printerDetailCameras.none')}
      {:else}
        {t('printerDetailCameras.noPicture')}
      {/if}
    </p>
  {/if}
  {#if camera.serving}
    {#if still !== undefined}
      <img
        src={still}
        alt={live ? '' : alt}
        class="absolute inset-0 size-full object-contain"
      />
    {/if}
    {#if live && view !== undefined}
      <div class="absolute inset-0">
        {@render view(camera)}
      </div>
    {:else if live && pictures.live !== undefined}
      <img
        src={pictures.live}
        {alt}
        class="absolute inset-0 size-full object-contain"
        onerror={fail}
      />
    {:else if live && watch !== undefined}
      <WebrtcView camera={camera.name} label={alt} {watch} onFail={fail} />
    {/if}
    {#if live}
      <Button
        variant="secondary"
        size="icon-md"
        class="absolute bottom-2 left-2 z-10"
        label={t('printerDetailCameras.stop', { camera: camera.name })}
        aria-pressed="true"
        onclick={toggle}
      >
        <SquareIcon aria-hidden="true" />
      </Button>
    {:else if livable}
      <Button
        variant="secondary"
        size="icon-lg"
        class="absolute top-1/2 left-1/2 z-10 -translate-1/2 shadow-md"
        label={t('printerDetailCameras.play', { camera: camera.name })}
        aria-pressed="false"
        onclick={toggle}
      >
        <PlayIcon aria-hidden="true" />
      </Button>
    {/if}
  {/if}
  {#if stalled}
    <p
      class="absolute inset-x-0 bottom-0 bg-background/80 px-3 py-2 text-sm text-destructive"
    >
      {t('printerDetailCameras.liveFailed')}
    </p>
  {/if}
  {@render children?.()}
</AspectRatio>
