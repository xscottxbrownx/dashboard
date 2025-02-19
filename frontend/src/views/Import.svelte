<div class="content">
    <Card footer={false}>
        <span slot="title">Import Settings, Tickets & Transcripts</span>
        <div slot="body" class="body-wrapper">
            <div class="section">
                <h3 class="section-title">Import Items</h3>

                <form>
                    <div class="row">
                        <div class="col-4">
                            <label class="form-label" for="import_data">
                                Data Export File (.zip)
                            </label>
                            <div class="col-1">
                                <input
                                    type="file"
                                    id="import_data"
                                    style="display: block; width: 90%;"
                                    accept=".zip"
                                />
                            </div>
                        </div>
                        <div class="col-4">
                            <label class="form-label" for="import_transcripts">
                                Transcripts Export File (.zip)
                            </label>
                            <div class="col-1">
                                <input
                                    type="file"
                                    id="import_transcripts"
                                    style="display: block; width: 90%;"
                                    accept=".zip"
                                />
                            </div>
                        </div>
                    </div>
                    <br />
                    <div class="row">
                        <div class="col-6">
                            <Button on:click={dispatchConfirm} icon={queryLoading ? "fa-solid fa-spinner fa-spin-pulse" : ""} disabled={queryLoading}>Confirm</Button>
                        </div>
                    </div>
                    <div class="row">
                        {#if queryLoading}
                        <div>
                            <br />
                            <br />
                            <p style="text-align: center;"><i class="fa-solid fa-spinner fa-spin-pulse"></i> We are currently loading your data in, please do not navigate away from this page.</p>
                        </div>
                        {/if}
                    </div>
                </form>
            </div>

            {#if dataReturned}
            <div class="section">
                <h2 class="section-title">Import Results</h2>
                <div class="row">
                    <p style="text-align: center;">Transcripts will be loaded in a separate request, and may take a few days to appear.</p>
                {#if resData.success.length > 0}
                <div class="col-3">
                    <div class="row">
                        <h3>Successful</h3>
                    </div>
                    <div class="row">
                        <!-- <ul style="color: lightgreen;"> -->
                        <ul>
                            {#each resData.success as item}
                                <li><i class="fa-solid fa-check"></i> {item}</li>
                            {/each}
                        </ul>
                    </div>
                </div>
                {/if}
                {#if resData.failed.length > 0}
                <div class="col-3">
                    <div class="row">
                        <h3>Failed</h3>
                    </div>
                    <div class="row">
                        <!-- <ul style="color: #ff7f7f;"> -->
                         <ul>
                            {#each resData.failed as item}
                                <li><i class="fa-solid fa-xmark"></i> {item}</li>
                            {/each}
                        </ul>
                    </div>
                </div>
                {/if}
                {#if resData.skipped.length > 0}
                <div class="col-3">
                    <div class="row">
                        <h3>Skipped</h3>
                    </div>
                    <div class="row">
                        <ul>
                            {#each resData.skipped as item}
                                <li><i class="fa-solid fa-minus"></i> {item}</li>
                            {/each}
                        </ul>
                    </div>
                </div>
                {/if}
                </div>
            </div>
            {/if}
        </div>
    </Card>
</div>
<script>
    import { createEventDispatcher } from "svelte";
    import { fade } from "svelte/transition";
    import Card from "../components/Card.svelte";
    import Button from "../components/Button.svelte";

    import Textarea from "../components/form/Textarea.svelte";

    import { setDefaultHeaders } from "../includes/Auth.svelte";
    import { notify, notifyError, notifySuccess } from "../js/util";
    import axios from "axios";
    import { IMPORT_API_URL } from "../js/constants";
    setDefaultHeaders();

    export let currentRoute;
    let guildId = currentRoute.namedParams.id

    let dataReturned = false;
    let resData = {
        success: [],
        failed: [],
        skipped: [],
    };

    let queryLoading = false;

    const dispatch = createEventDispatcher();

    function dispatchClose() {
        dispatch("close", {});
    }


    async function dispatchConfirm() {
        let dataFileInput = document.getElementById("import_data");
        let transcriptFileInput = document.getElementById("import_transcripts");

        if (
            dataFileInput.files.length === 0 &&
            transcriptFileInput.files.length === 0
        ) {
            notifyError(
                "Please select a file to import, at least one of data or transcripts must be provided",
            );
            return;
        }

        const frmData = new FormData();
        if (dataFileInput.files.length > 0) {
            frmData.append("data_file", dataFileInput.files[0]);
        }

        queryLoading = true;
        setTimeout(() => {
            if (queryLoading) {
                notify(
                    "Importing...",
                    "Your data is taking longer than expected to import, if you uploaded transcripts, please wait until you get an import successful message before navigating away from this page.",
                );
            }
        }, 60 * 1000);

        if (transcriptFileInput.files.length > 0) {
            const presignRes = await axios.get(`${IMPORT_API_URL}/api/${guildId}/import/presign?file_size=${transcriptFileInput.files[0].size}`);
            if (presignRes.status !== 200) {
                notifyError(`Failed to upload transcripts: ${presignRes.data.error}`);
                queryLoading = false;
                return;
            }
            
            await fetch(presignRes.data.url, {
                method: "PUT",
                body: transcriptFileInput.files[0],
                headers: {
                    "Content-Type": transcriptFileInput.files[0].type,
                },
            }).then((res) => {
                if (res.status !== 200) {
                    notifyError(`Failed to upload transcripts: ${res.data.error}`);
                    queryLoading = false;
                    return;
                }

                notifySuccess("Transcripts uploaded successfully");
            });
        }

        if (dataFileInput.files.length > 0) {
            const res = await axios.post(
                `${IMPORT_API_URL}/api/${guildId}/import`,
                frmData,
                {
                    headers: {
                        "Content-Type": "multipart/form-data",
                    },
                },
            );
            if (res.status !== 200) {
                notifyError(`Failed to import settings: ${res.data.error}`);
                queryLoading = false;
                return;
            }
            dataReturned = true;
            resData = res.data;
        }

        queryLoading = false;

        dispatchClose();
        notifySuccess(
            "Imported settings successfully - Your transcripts will be processed separately and may take some time to appear.",
        );
    }

    function handleKeydown(e) {
        if (e.key === "Escape") {
            dispatchClose();
        }
    }
</script>
<style>
    .content {
        display: flex;
        width: 100%;
        height: 100%;
    }

    .body-wrapper {
        display: flex;
        flex-direction: column;
        width: 100%;
        height: 100%;
        padding: 1%;
    }

    .section {
        display: flex;
        flex-direction: column;
        width: 100%;
        height: 100%;
    }

    .section:not(:first-child) {
        margin-top: 2%;
    }

    .section-title {
        font-size: 36px;
        font-weight: bolder !important;
    }

    h3 {
        font-size: 28px;
        margin-bottom: 4px;
    }

    .row {
        display: flex;
        flex-direction: row;
        width: 100%;
        height: 100%;
    }

    ul {
        margin: 0;
        padding: 0;
    }

    li {
        list-style-type: none;
    }

    .manage {
        display: flex;
        flex-direction: row;
        justify-content: space-between;
        width: 100%;
        height: 100%;
        margin-top: 12px;
    }

    .col {
        display: flex;
        flex-direction: column;
        align-items: center;
        width: 49%;
        height: 100%;
    }

    table.nice > tbody > tr:first-child {
        border-top: 1px solid #dee2e6;
    }

    .user-select {
        display: flex;
        flex-direction: row;
        justify-content: space-between;
        width: 100%;
        height: 100%;
        margin-bottom: 1%;
    }

    @media only screen and (max-width: 950px) {
        .manage {
            flex-direction: column;
        }

        .col {
            width: 100%;
        }
    }
</style>
