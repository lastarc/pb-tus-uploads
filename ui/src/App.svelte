<script lang="ts">
    import {onMount} from 'svelte'
    import {writable} from 'svelte/store'
    import PocketBase, {type AuthModel, type RecordModel} from 'pocketbase'
    import Uppy from "@uppy/core";
    import DragDrop from "@uppy/drag-drop";
    import StatusBar from "@uppy/status-bar";
    import Tus from "@uppy/tus";

    import '@uppy/core/dist/style.css'
    import '@uppy/drag-drop/dist/style.css';
    import '@uppy/status-bar/dist/style.css';

    const pb = new PocketBase('/');
    const redirectUrl = writable('');
    const user = writable<AuthModel>(null)
    const uploadHistory = writable<RecordModel[]>([]);
    const error = writable('')

    let uppyElement: HTMLDivElement;
    let statusBarElement: HTMLDivElement;

    const formatBytes = (bytes: number) => {
        if (bytes === 0) return '0 Bytes';
        const k = 1024;
        const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        const size = parseFloat((bytes / Math.pow(k, i)).toFixed(1));
        return `${size}${sizes[i]}`;
    }

    const login = async () => {
        const authMethods = await pb.collection('users').listAuthMethods();
        const provider = authMethods.authProviders.find(i => i.name.toLowerCase() === 'discord');
        if (!provider) {
            return
        }
        localStorage.setItem('provider', JSON.stringify(provider));
        window.location.href = provider.authUrl + $redirectUrl
    }

    const oauthCallback = async ({code, state}: {
        code: string,
        state: string,
    }) => {
        try {
            const provider = JSON.parse(localStorage.getItem('provider') || '{}')
            if (provider.state !== state) {
                throw "State parameters don't match.";
            }

            const authData = await pb.collection('users').authWithOAuth2Code(
                provider.name,
                code,
                provider.codeVerifier,
                $redirectUrl,
                { emailVisibility: false }
            )
            console.log(authData)
            localStorage.setItem('provider', '')
            location.href = $redirectUrl
        } catch (e) {
            error.set(e);
        } finally {
        }
    }

    const logout = () => {
        pb.authStore.clear();
        const keys = Object.keys(localStorage).filter((key) => key.startsWith('tus::'));
        keys.forEach((key) => localStorage.removeItem(key));
        user.set(pb.authStore.model)

        // force reload bc why not
        location.href = $redirectUrl
    }

    const uppy = new Uppy()

    $: {
        if (uppyElement && statusBarElement) {
            uppy.use(DragDrop, {target: uppyElement})
            uppy.use(StatusBar, {target: statusBarElement})
        }
    }

    uppy.use(Tus, {
        endpoint: '/uploads',
        headers: {
            authorization: `Bearer ${pb.authStore.token}`,
        },
        chunkSize: 5 * 1024 * 1024, // 5MB
    })

    uppy.addPostProcessor(async (fileIDs) => {
        console.log(fileIDs)

        await Promise.all(fileIDs.map(id => {
            const file = uppy.getFile(id);
            console.log(file);
            if (!("uploadURL" in file) || typeof file.uploadURL !== 'string') {
                throw "invalid uploadURL"
            }
            const uploadUrl = new URL(file.uploadURL);
            const uid = uploadUrl.pathname.slice(uploadUrl.pathname.lastIndexOf('/') + 1);

            return pb.collection('accessRefs').create({
                upload: uid,
                user: pb.authStore.model?.id,
            })
        }))
    });

    const updateUploadHistory = () =>
        pb.collection('uploads').getFullList({
            expand: "accessRefs_via_upload",
            filter: "current_offset = size",
            sort: "-created",
        }).then(uploadHistory.set)

    onMount(() => {
        try {
            pb.authStore.isValid && pb.collection('users').authRefresh()
                .then(() => user.set(pb.authStore.model));
        } catch (_) {
            pb.authStore.clear();
            user.set(null)
        }

        const _redirectUrl = new URL(location.href)
        _redirectUrl.search = ''
        redirectUrl.set(_redirectUrl.toString())

        const params = (new URL(window.location.href)).searchParams
        const code = params.get('code')
        const state = params.get('state')
        if (code && state) {
            oauthCallback({code, state})
        }

        pb.collection('uploads').subscribe('*', (e) => {
            if (e.record.collectionName === 'uploads' &&
                e.action === 'update' &&
                e.record["current_offset"] !== e.record["size"]) {
                return
            }
            updateUploadHistory()
        });
        pb.collection('accessRefs').subscribe('*', updateUploadHistory);

        () => {
            pb.collection('uploads').unsubscribe('*');
            pb.collection('accessRefs').unsubscribe('*');
        }
    })

    $: {
        if ($user) {
            updateUploadHistory();
        } else {
            uploadHistory.set([])
        }
    }
</script>

<header class="container">
    <nav>
        <ul>
        </ul>
        <ul>
            {#if $user}
                <li><strong>{$user.username}</strong></li>
                <li>
                    <button on:click={logout}>Logout</button>
                </li>
            {:else}
                <li>
                    <button on:click={login} class="fancy-animation">Login</button>
                </li>
            {/if}
        </ul>
    </nav>
</header>
<main class="container">
    {#if $error}
        <section class="error">
            <pre>{$error}</pre>
        </section>
    {/if}

    {#if $user}
        <section>
            <h1>Upload new file</h1>

            <div bind:this={uppyElement}></div>

            <div bind:this={statusBarElement}></div>
        </section>
        <section>
            <h1>History</h1>

            <div class="overflow-auto" style="max-width: 100%">
                <table class="striped">
                    <thead>
                    <tr>
                        <th scope="col">Filename</th>
                        <th scope="col">Mime Type</th>
                        <th scope="col">Size</th>
                    </tr>
                    </thead>
                    <tbody data-var-uploadHistory>
                    {#each $uploadHistory as item}
                        {@const link = item.expand?.accessRefs_via_upload?.length > 0 ? `/accref/${item.expand?.accessRefs_via_upload[0].id}` : null}
                        <tr>
                            <th scope="row">
                                <a href={link || '#'}>
                                    {item['filename']}
                                </a>
                            </th>
                            <td>{item['mime_type']}</td>
                            <td>{formatBytes(item['size'])}</td>
                        </tr>
                    {:else}
                        <tr>
                            <td colspan="3" style="text-align: center;">No items</td>
                        </tr>
                    {/each}
                    </tbody>
                </table>
            </div>
        </section>
    {:else}
        <section style="text-align: center; padding-top: 40vh">
            <p>
                We were awaiting your arrival, please identify yourself to proceed!
            </p>
        </section>
    {/if}
</main>

<footer class="container">
    <small data-tooltip="Ehe~">
        Developed by Arc
    </small>
</footer>

<style>
    :global(body) {
        background-color: var(--pico-background-color);
        color: var(--pico-color);
    }

    @font-face {
        font-family: 'Monogram';
        src: url('/static/monogram.ttf') format('truetype');
    }

    main {
        min-height: calc(100vh - 120px);
    }

    footer {
        font-family: 'Monogram', monospace;
        text-align: center;
        text-decoration: underline;
        text-decoration-style: dotted;
        color: rgba(105, 105, 105, 0.3);
    }

    footer small {
        font-size: 1rem;
        cursor: help;
    }

    .fancy-animation {
        animation: fancy-animation 1s infinite;
    }

    .fancy-animation:hover {
        animation: intense-fancy-animation 0.8s infinite;
    }

    @keyframes fancy-animation {
        0% {
            transform: scale(100%);
        }
        50% {
            transform: scale(110%);
        }
        100% {
            transform: scale(100%);
        }
    }

    @keyframes intense-fancy-animation {
        0% {
            transform: scale(100%);
        }
        50% {
            transform: scale(130%);
        }
        100% {
            transform: scale(100%);
        }
    }

    .error * {
        color: red;
    }
</style>
